# node-local-hotplug — Attach / Detach Flow

This document is the definitive reference for how `pkg/virt-handler/node-local-hotplug`
attaches and detaches a host-local block device or file to a running VMI. It
covers the gRPC contract, the VMI patch sequence, the host-side mount
operations, the libvirt interactions that happen *implicitly* via virt-handler's
existing reconcile loop, the state machine on `vmi.Status.VolumeStatus`, the
recovery story across virt-handler restarts, and the cross-component
interactions that have produced subtle bugs during PoC testing.

If you are touching anything in this package, read at least sections
[Component map](#component-map), [Attach](#attachdevice), [Detach](#detachdevice),
and [State machine](#volume-status-state-machine).

---

## Why a separate path

KubeVirt's existing hotplug pipeline (`pkg/virt-handler/hotplug-disk`,
`pkg/virt-controller/...`) is built around an *attachment pod*: virt-controller
spawns a sidecar pod with the PVC mounted, virt-handler discovers it via
`findmnt`, and bind-mounts the PVC content into the launcher pod's
`hotplug-disks` emptyDir. That pipeline does not work for resources that
have no PVC representation:

- a raw host block device (`/dev/loopN`, an NVMe namespace, a SCSI LUN
  carved out by an external CSI driver),
- a host file the cluster administrator wants to expose for ephemeral
  scratch storage,
- anything whose lifecycle is owned outside Kubernetes' storage stack.

`node-local-hotplug` is a tightly scoped gRPC service hosted **inside
virt-handler** that takes a host path and a target VMI, validates both,
**patches the VMI spec to add the corresponding `Volume` + `Disk`**
(plus a `VolumeStatus` entry for observability), performs the host-side
staging (`mknod` for block, bind-mount for file) into the launcher pod's
`hotplug-disks` emptyDir, and then **lets the spec change ride
virt-handler's existing informer-driven reconcile loop into a
`SyncVirtualMachine` call** on the launcher, which is what ultimately
runs the converter and calls libvirt's `AttachDeviceFlags`. Detach is
the reverse — single patch removing the entries, then wait for libvirt
to actually release the disk before tearing down the host mount.

The service is intentionally a **spec-mutator + node-local mount
helper**. It does NOT call `SyncVirtualMachine` on virt-launcher
itself. virt-handler's `VirtualMachineController` already owns that
gRPC; racing it for ownership of the launcher sync produced more bugs
than it solved during the initial design. Trigger semantics are: a
patch to `vmi.Spec.Volumes` / `vmi.Spec.Domain.Devices.Disks` fires the
informer Update event in `VirtualMachineController`, which enqueues the
VMI; the next reconcile tick calls `SyncVirtualMachine`; the launcher's
`SyncVMI` rebuilds the desired domain via the converter and diffs it
against the live libvirt XML; the new disk is in the diff →
`AttachDeviceFlags` is invoked. See [Step 4 — async, owned by
virt-handler](#step-4--async-owned-by-virt-handler) for the full
sequence diagram.

---

## Component map

```
         ┌──────────────────────┐
         │  external client     │  e.g. CSI driver, admission proxy,
         │  (any process on the │       cluster operator script
         │   node that can talk │
         │   to the unix socket)│
         └──────────┬───────────┘
                    │ gRPC over /var/run/kubevirt/node-local-hotplug.sock
                    ▼
   ┌────────────────────────────────────────────────────────────┐
   │ virt-handler pod (DaemonSet, hostPID, hostNetwork)          │
   │                                                            │
   │  ┌──────────────────────────────────────────────────────┐  │
   │  │ pkg/virt-handler/node-local-hotplug                  │  │
   │  │   server.go    : grpc.Server + listener              │  │
   │  │   service.go   : Attach/Detach orchestration         │  │
   │  │   validator.go : pre-flight checks                   │  │
   │  │   patcher.go   : JSONPatch builders                  │  │
   │  │   mounter.go   : host-side mknod / bind-mount        │  │
   │  │   errors.go    : coded errors → gRPC response codes  │  │
   │  └──────────────────────────────────────────────────────┘  │
   │                       │                                    │
   │                       │ patches VMI                        │
   │                       ▼                                    │
   │  ┌──────────────────────────────────────────────────────┐  │
   │  │ apiserver  (VMI CRD has NO /status subresource;      │  │
   │  │             a single JSONPatch updates spec+status)  │  │
   │  └──────────────────────────────────────────────────────┘  │
   │                       │                                    │
   │                       │ informer Update                    │
   │                       ▼                                    │
   │  ┌──────────────────────────────────────────────────────┐  │
   │  │ pkg/virt-handler/vm.go                                │  │
   │  │   reconcile loop sees the new disk in spec, calls    │  │
   │  │   client.SyncVirtualMachine(vmi) on virt-launcher    │  │
   │  └──────────────────────────────────────────────────────┘  │
   │                       │                                    │
   └───────────────────────┼────────────────────────────────────┘
                           ▼
   ┌────────────────────────────────────────────────────────────┐
   │ virt-launcher pod (compute container, runs QEMU as qemu)    │
   │                                                            │
   │   converter builds DomainSpec from VMI                     │
   │   syncDisks() diffs DomainSpec vs live libvirt XML and     │
   │   calls dom.AttachDeviceFlags(...) for each new disk       │
   │   whose backing path passes checkIfDiskReadyToUse()        │
   │                                                            │
   │   Source path expected in the launcher's view:             │
   │     block: /var/run/kubevirt/hotplug-disks/<vol>           │
   │     file : /var/run/kubevirt/hotplug-disks/<vol>.img       │
   │                                                            │
   │   The above directory is the launcher pod's emptyDir       │
   │   `hotplug-disks`, mounted with HostToContainer            │
   │   propagation. virt-handler stages the host resource into  │
   │   the *kubelet view* of that emptyDir on the host:         │
   │     /var/lib/kubelet/pods/<launcher-uid>/volumes/          │
   │       kubernetes.io~empty-dir/hotplug-disks/<vol>[.img]    │
   │   and the propagation makes it appear inside the launcher. │
   └────────────────────────────────────────────────────────────┘
```

Key invariants worth memorising:

- **virt-handler runs with `HostPID: true` and `mountPropagation: Bidirectional`
  on `/var/lib/kubelet`.** Host paths are reachable through `/proc/1/root`
  (see `pkg/util.HostRootMount`); mounts performed via virt-chroot land in
  the host mount namespace.
- **VMI CRD has no `/status` subresource.** A single JSONPatch on the main
  resource may write spec, status, or both. We exploit this to land
  spec+status changes atomically in a single round trip (see `patcher.go`
  comments).
- **virt-launcher's `hotplug-disks` emptyDir uses `HostToContainer` propagation.**
  Bind-mounts created on the host inside the kubelet's view of that emptyDir
  propagate INTO the launcher. They do NOT propagate the other way; that
  matters for unmount ordering.

---

## gRPC contract (summary)

Defined in `v1/nodelocalhotplug.proto`. Two RPCs:

```
service NodeLocalHotplug {
  rpc AttachDevice(AttachDeviceRequest) returns (AttachDeviceResponse);
  rpc DetachDevice(DetachDeviceRequest) returns (DetachDeviceResponse);
}
```

Both responses carry `success bool`, `message string`, and a structured
`code NodeLocalHotplugErrorCode` for machine-readable failure
classification. `success=false` is a **business** failure (validation,
conflict, mount); transport-level errors (cancelled, deadline-exceeded)
surface as gRPC status codes per usual.

The unix socket is at `/var/run/kubevirt/node-local-hotplug.sock` inside
virt-handler's container. Callers reach it by mounting the same socket
into their own pod or by `kubectl exec`'ing into virt-handler with a small
client like `cmd/nlh-cli`.

---

## VolumeStatus state machine

The `node-local-hotplug` flow drives a `VolumeStatus` entry through the
following phases. Other phase values from `staging/.../v1/types.go` exist
but are owned by other code paths (PVC hotplug attachment-pod state) and
the node-local-hotplug controller does not write them.

```
         AttachDevice
            ┌──┐
            ▼  │
       ┌────────────┐  intent patch (1)
       │   Bound    │◄───────────────────  buildAttachIntentPatch
       └─────┬──────┘  reason=VolumeBound
             │         message="host path validated; bind-mount pending"
             │
             │ MountFile / MountBlock succeeds
             │
             │ mounted patch (2) — buildAttachMountedPatch
             ▼
       ┌────────────┐
       │MountedToPod│  reason=MountedToPod
       └─────┬──────┘  message="device exposed under launcher hotplug-disks dir"
             │
             │ virt-handler reconcile → SyncVirtualMachine → virt-launcher
             │ syncDisks → libvirt AttachDeviceFlags succeeds
             │
             │ virt-handler's existing updateVolumeStatusesFromDomain sees
             │ Target!="" in domain XML; rewrites the entry
             ▼
       ┌────────────┐
       │   Ready    │  reason=VolumeReady, target="vd<x>"
       └─────┬──────┘  message="Successfully attach hotplugged volume…"
             │
             │ DetachDevice — applyDetachPatch
             │ (single patch: removes Volume, Disk, VolumeStatus together)
             ▼
       <entry deleted from vmi.Status.VolumeStatus>
             │
             │ virt-handler reconcile → SyncVirtualMachine → virt-launcher
             │ syncDisks → libvirt DetachDeviceFlags succeeds
             │
             │ Domain informer no longer reports the disk
             │
             │ Service.waitDomainDiskAbsent unblocks
             │
             │ UnmountFile / UnmountBlock removes the host-side staging
```

Notes on the transitions:

- The `MountedToPod` message you see in real clusters is usually
  virt-handler's text ("`Volume scratch has been mounted in virt-launcher
  pod`"), not ours, because virt-handler's reconcile races us and rewrites
  the message after observing `IsMounted()=true`. The phase transition is
  the source of truth, not the message.
- The `Bound → MountedToPod` transition is idempotent: if virt-handler's
  reconcile happens to advance the phase first, our `applyAttachMountedPatch`
  is a no-op (`vs.Phase == HotplugVolumeMounted` short-circuits).
- We never write `AttachedToNode`; that's a PVC-attachment-pod artefact.

---

## AttachDevice

### Pre-flight

`Service.AttachDevice(ctx, req)`:

1. **VMI lookup** via the local informer cache (`vmiStore`). The cache is
   filtered to VMIs whose `nodeName == this node`, so a request for a VMI
   on another node returns `VMI_NOT_FOUND` with `success=false`. Race
   window: an in-flight migration may briefly have the cache lag — the
   caller must retry.
2. **Per-VMI lock** (`Service.lockVMI(vmi.UID)`) is acquired. The lock is
   reference-counted and released when the call returns, so two concurrent
   `Attach`/`Detach` calls for the same VMI serialise. Two calls for
   different VMIs proceed in parallel.
3. **`validateAttach(req, vmi)`** runs:
   - VMI must be `Running` (not in a final phase, not migrating).
   - The named volume must not already exist *with a different shape*
     (see classification below).
   - For block: host path must exist via `os.Stat(util.HostRootMount + path)`
     and be a block device.
   - For file: host path must exist and be a regular file.
   - The same `device_path` must not already be referenced by another
     `node-local` volume on the VMI (the G9 check in
     `findVolumeByNodeLocalPath`).
   - `target_bus` must be `VIRTIO` or `SCSI` (SATA was intentionally
     dropped to match the existing admitter).
4. **`classifyAttachAgainstExisting(req, vmi)`** returns one of:
   - `attachFresh`     — nothing for this volume name yet, do all three steps.
   - `attachIdempotent` — same volume name + identical source already in
     place. Skip step 1 (intent patch), still re-assert mount and the
     mounted patch (cheap no-ops on a healthy state, recovery after
     virt-handler restart).
   - error                — volume name exists but with a different source
     (e.g. caller raced themselves with two different paths). Returns
     `VOLUME_CONFLICT`.

### Step 1 — intent patch (only on `attachFresh`)

`applyAttachIntentPatch(ctx, vmi, req)` builds a single JSONPatch payload
that adds:

- `/spec/volumes/-`               → the new `v1.Volume` with
                                       `VolumeSource.NodeLocalDevice`
- `/spec/domain/devices/disks/-`  → the new `v1.Disk` with bus + serial
- `/status/volumeStatus/-`        → entry with `phase=Bound`

Each `add` is preceded by a `test` op against the slice's current value so
that a concurrent unrelated write (interface stats, conditions, anything
under `/status`) does NOT cause us to overwrite it. On 409 Conflict or
422 Invalid (the apiserver's response when a `test` op misses), we re-GET
the VMI and rebuild the patch — see `isPatchRetriable` in `service.go`.

The function returns the **patched VMI as the apiserver returned it**
(not the pre-patch input). Steps 2 and 3 must use that returned VMI;
re-using the pre-patch informer copy will leave them looking at stale
`Status.VolumeStatus` data and `findVolumeStatus` will return "not found"
on data that is, at that exact moment, live on the apiserver. (This is
the bug the comment block above `current := vmi` in `AttachDevice`
references; it bit us in early manual testing.)

### Step 2 — host-side staging

`mountForAttach(current, req)` dispatches to the mounter:

- `MountBlock(vmi, vol, hostDevPath)`:
  1. `findVirtlauncherUID(vmi)` — pick the local launcher pod UID. We
     filter `vmi.Status.ActivePods[uid] == m.host` first, which avoids the
     misfire mode in the existing PVC mounter where a leftover pod
     directory on this node could falsely match a remote pod.
  2. `hostBlockMajorMinor(hostDevPath)` — `os.Stat(util.HostRootMount + path)`
     to extract `Rdev` and verify it is actually a block device.
  3. `mknod` the device into the launcher pod's `hotplug-disks` emptyDir
     via `safepath.MknodAtNoFollow`. The new inode lives inside the
     launcher's emptyDir and inherits the directory's SELinux MCS — so
     SELinux access "just works" for block.
  4. cgroup `devices.allow` rule for `(major, minor, rwm)` is added to
     the launcher's cgroup via the injected `CgroupManagerProvider`.
     The provider closure carries the hypervisor device name and
     `AllowEmulation` flag from the cluster config; passing an empty
     hypervisor name causes `cgroup/util.go` to construct the invalid
     path `/dev/`, which `safepath` rejects (we hit this in early
     testing — see commit history).
  5. Set ownership to qemu via `ownershipManager.SetFileOwnership`.

- `MountFile(vmi, vol, hostFilePath, readonly)`:
  1. `findVirtlauncherUID(vmi)` as above.
  2. `os.Stat(util.HostRootMount + path)` to verify the source is a
     regular file.
  3. Resolve target via `hotplugDiskManager.GetFileSystemDiskTargetPathFromHostView`
     (this `Touch`es a placeholder file at `<launcher pod hotplug-disks>/<vol>.img`).
  4. Bind-mount the host file onto the placeholder via
     `virtchroot.MountChroot`. virt-chroot strips the `/proc/1/root`
     prefix and runs `nsenter --mount=/proc/1/ns/mnt mount -o bind ...`
     in the host mount namespace. The bind mount propagates INTO the
     launcher because the emptyDir was mounted with `HostToContainer`.
  5. **SELinux relabel** of the bind-mount target via
     `selinux.RelabelFilesUnprivileged` to
     `system_u:object_r:container_file_t:s0` (no MCS categories). Bind
     mounts share inodes, so without this the launcher (running with a
     per-pod MCS like `s0:c86,c796`) cannot open a host file labelled
     with any other MCS — `checkIfDiskReadyToUse` would silently return
     false and `syncDisks` would skip the disk with no log line. `s0`
     with no categories is dominated by every per-pod MCS, so any
     launcher can read it.
  6. Set ownership to qemu.

If the mount fails AND we landed the intent patch in this same call, we
roll the intent patch back via `rollbackAttachIntent` so that the next
retry sees a clean slate (no orphaned spec entry pointing at a volume we
never staged).

### Step 3 — mounted patch

`applyAttachMountedPatch(ctx, current, vol)`:

1. `findVolumeStatus(current, vol)` locates the entry we added in step 1.
   - If missing (cached VMI is stale, or another controller wiped the
     entry), do a fresh `Get` and retry. We wrap the
     `volumeStatus entry "%q" not found` error as `errVolumeStatusStale`
     so `isPatchRetriable` matches it via `errors.Is`, which causes
     `retry.OnError` to rerun the closure with the freshly-GET'd VMI. If
     after `retry.DefaultRetry` (5 steps) we still can't find the entry,
     we surface PATCH_FAILED — the operator gets to decide whether to
     retry the whole `AttachDevice` or wait for a virt-handler restart
     to invoke `Recover()`.
2. If the entry's phase is already `MountedToPod` or `Ready`
   (virt-handler's reconcile beat us, or this is an idempotent re-attach
   after a virt-handler restart), return `nil` — no patch needed.
3. Otherwise build the second JSONPatch:
   - `test` `/status/volumeStatus/<idx>` is the existing `Bound` entry
   - `replace` `/status/volumeStatus/<idx>` with the same entry but
     `phase=MountedToPod`, updated reason/message.

   On 409/422 we re-GET, re-classify, and retry; same retry loop as the
   intent patch.

### Step 4 — async, owned by virt-handler

This is the most-asked clarification: **what triggers libvirt to actually
attach the disk?** We do NOT call `SyncVirtualMachine` ourselves. The
intent patch from step 1 changed `/spec/volumes` AND
`/spec/domain/devices/disks` (it's not a status-only change), and that
spec change is what drives the rest. The chain in detail:

```
AttachDevice                     virt-handler                         virt-launcher                 libvirt
    │                                  │                                    │                          │
    │ JSONPatch                        │                                    │                          │
    │  /spec/volumes/-                 │                                    │                          │
    │  /spec/domain/devices/disks/-    │                                    │                          │
    │  /status/volumeStatus/-          │                                    │                          │
    ├────────────► apiserver ──────────► VMI informer UpdateFunc            │                          │
    │                                  │                                    │                          │
    │                                  │ enqueueVMI(key)                    │                          │
    │                                  │                                    │                          │
    │                                  │ workqueue → sync(key, vmi)         │                          │
    │                                  │                                    │                          │
    │                                  │ shouldUpdate = true                │                          │
    │                                  │   (VMI Running, phase matches)     │                          │
    │                                  │                                    │                          │
    │                                  │ processVmUpdate(vmi, domain)       │                          │
    │                                  │   → vmUpdateHelperDefault          │                          │
    │                                  │   → handleRunningVMI               │                          │
    │                                  │   → c.syncVirtualMachine(client,   │                          │
    │                                  │                          vmi, …)   │                          │
    │                                  │                                    │                          │
    │                                  │ ──── client.SyncVirtualMachine ──► │                          │
    │                                  │      (cmd-client unix socket)      │                          │
    │                                  │                                    │                          │
    │                                  │                                    │ cmd-server.SyncVMI       │
    │                                  │                                    │   → LibvirtDomainManager │
    │                                  │                                    │     .SyncVMI(vmi, …)     │
    │                                  │                                    │                          │
    │                                  │                                    │ generateConverterContext │
    │                                  │                                    │   builds HotplugVolumes  │
    │                                  │                                    │   from VolumeStatus      │
    │                                  │                                    │   where HotplugVolume    │
    │                                  │                                    │   != nil                 │
    │                                  │                                    │                          │
    │                                  │                                    │ converter rebuilds       │
    │                                  │                                    │ domain.Spec.Devices.Disks│
    │                                  │                                    │ from vmi.Spec.Domain.    │
    │                                  │                                    │ Devices.Disks (now       │
    │                                  │                                    │ includes our new disk)   │
    │                                  │                                    │                          │
    │                                  │                                    │ syncDisks(domain,        │
    │                                  │                                    │           oldSpec, dom,  │
    │                                  │                                    │           vmi)           │
    │                                  │                                    │                          │
    │                                  │                                    │ getAttachedDisks =       │
    │                                  │                                    │   converter desired      │
    │                                  │                                    │   ∖ live libvirt XML     │
    │                                  │                                    │   = [scratch]            │
    │                                  │                                    │                          │
    │                                  │                                    │ checkIfDiskReadyToUse    │
    │                                  │                                    │   /var/run/kubevirt/     │
    │                                  │                                    │   hotplug-disks/<vol>    │
    │                                  │                                    │   → true                 │
    │                                  │                                    │                          │
    │                                  │                                    │ dom.AttachDeviceFlags ──►│ adds disk
    │                                  │                                    │  (xml, AFFECT_LIVE|      │ to live
    │                                  │                                    │   AFFECT_CONFIG)         │ + persistent
    │                                  │                                    │                          │ XML
    │                                  │                                    │                          │
    │                                  │ ◄──── domain notify socket ───────────────────────────────────│
    │                                  │   (libvirt event: device added)    │                          │
    │                                  │                                    │                          │
    │                                  │ Domain informer cache updated      │                          │
    │                                  │                                    │                          │
    │                                  │ next reconcile tick:               │                          │
    │                                  │ updateVolumeStatusesFromDomain     │                          │
    │                                  │   diskDeviceMap[<vol>] = "vd<x>"   │                          │
    │                                  │   updateHotplugVolumeStatus:       │                          │
    │                                  │     Target != "" → Phase=Ready     │                          │
    │                                  │                                    │                          │
    │                                  │ status update → apiserver          │                          │
```

In other words:

1. **`/spec/volumes` and `/spec/domain/devices/disks` are the trigger.**
   The status entry is for observability and to satisfy the converter's
   `hotplugReady` gate; it's the spec mutation that informs virt-handler
   "there's new work to sync to the launcher".
2. **Why the informer fires at all.** virt-handler's VMI informer is
   filtered to VMIs whose `Status.NodeName == this node`. Any patch to
   our VMI — spec or status, doesn't matter — triggers a watch event,
   which becomes a `cache.UpdateFunc` callback, which calls
   `enqueueVMI(key)`. This is plain client-go shared-informer plumbing;
   we just rely on it.
3. **Why `processVmUpdate` decides to re-sync.** The dispatch in
   `vm.go::sync` is gated by `shouldUpdate = vmi.Status.Phase == phase`.
   For a Running VMI with a Running domain that condition is true on
   every reconcile, so any informer wakeup of a Running VMI calls
   `processVmUpdate` → `vmUpdateHelperDefault` → `syncVirtualMachine`.
   There is no "did anything actually change" check; the sync is
   declarative — it diffs converter-desired vs libvirt-live and acts
   only on the delta.
4. **The converter is the diff machine.** `Convert_v1_Hotplug_Volume_To_api_Disk`
   contains our G2 arm: it dispatches `source.NodeLocalDevice` to
   `Convert_v1_Hotplug_BlockVolumeSource_To_api_Disk` or
   `Convert_v1_Hotplug_FilesystemVolumeSource_To_api_Disk` based on
   `Format`. The output `api.Disk` has `Source.Dev` or `Source.File`
   pointing at `/var/run/kubevirt/hotplug-disks/<vol>[.img]` (a path
   inside the launcher container). `disksource.Resolve(disk).IsHotplugDisk()`
   returns true because the prefix matches `v1.HotplugDiskDir`.
5. **The hotplugReady gate.** The converter only appends the disk to
   `domain.Spec.Devices.Disks` when the matching `VolumeStatus.Phase`
   is `MountedToPod` or `VolumeReady`. This is why the mounted patch in
   step 3 matters: a disk in spec but with status still at `Bound` is
   intentionally invisible to the converter, so virt-launcher won't try
   to attach it before the host-side mount has actually landed.
6. **Silent skip in `checkIfDiskReadyToUse`.** Once the disk reaches the
   converter's output, `getAttachedDisks` produces it as a candidate to
   attach. `checkIfDiskReadyToUse(ds.BackendPath())` does `os.Stat` +
   `os.OpenFile(O_RDWR)` from inside the launcher container. If either
   fails (file missing, permission denied, SELinux deny) it returns
   `(false, nil)` and the loop in `syncDisks` `continue`s with no log
   line. The positive signal that attach was attempted is the V(1) log
   `Attaching disk <vol>, target vd<x>` immediately followed by an
   `AttachDeviceFlags` libvirt call. If you see neither, your
   `MountedToPod` will never advance to `Ready`. See the SELinux
   section for the most common cause.
7. **`AttachDeviceFlags`.** With both `AFFECT_LIVE` and `AFFECT_CONFIG`
   flags, libvirt updates both the running QEMU monitor and the
   persistent domain XML. The disk is now visible to the guest at the
   allocated `vdX` (or `sdX` for SCSI).

### Step 5 — phase reaches `Ready`

`virt-handler`'s domain informer sees the updated libvirt XML (now with
the new `<disk target=vdX/>`). On the next tick of
`updateVolumeStatusesFromDomain`:

- `diskDeviceMap[<vol>] = "vdX"`
- The volumeStatus's `Target` is set to `"vdX"`
- `updateHotplugVolumeStatus` sees `Target != ""` → phase becomes
  `VolumeReady`, message becomes `"Successfully attach hotplugged
  volume <vol> to VM"`.

This is owned entirely by virt-handler's existing reconcile; we don't
participate. Callers polling on `vmi.Status.VolumeStatus[vol].Phase ==
"Ready"` get a definitive "the guest can now address this disk" signal.

### Failure modes during attach

| Failure                                               | Code                          | Recovery                         |
|-------------------------------------------------------|-------------------------------|----------------------------------|
| VMI not found in local cache                          | `VMI_NOT_FOUND`               | client retry with backoff        |
| VMI not Running, or migrating                         | `INVALID_REQUEST`             | client retry once VMI settles    |
| Host path missing / wrong type                        | `INVALID_REQUEST`             | client fixes the path, retries   |
| Same volume name, different source                    | `VOLUME_CONFLICT`             | client picks a new name          |
| Same `device_path` already mounted under another name | `VOLUME_CONFLICT`             | client renames or detaches first |
| `target_bus` not VIRTIO/SCSI                          | `INVALID_REQUEST`             | client fixes the bus             |
| Patch round 1 fails persistently                      | `PATCH_FAILED`                | client retries; virt-handler `Recover()` will re-assert next restart |
| Mount fails (mknod EPERM, bind-mount EBUSY, …)        | `MOUNT_FAILED`                | client retries; intent patch is rolled back |
| Patch round 2 fails persistently                      | `PATCH_FAILED`                | host stage stays in place; `Recover()` will advance Bound→MountedToPod next restart |
| libvirt AttachDeviceFlags errors                      | (no code — async)             | virt-launcher logs; not surfaced via gRPC |
| `checkIfDiskReadyToUse` silently returns false        | (no code — async)             | watch launcher logs for missing `Attaching disk` line; usually SELinux or perms |

The last two are *not* visible on `AttachDeviceResponse`: by the time
libvirt's reconcile runs, our gRPC call has already returned. The caller
must poll `vmi.Status.VolumeStatus[vol].Phase` to confirm reachability.
This asymmetry is documented in the gRPC response message:

> `volume "<v>" staged for attach (path=…); libvirt attach is asynchronous,
>  watch VolumeStatus[<v>] for VolumeReady`

---

## DetachDevice

`Service.DetachDevice(ctx, req)` runs symmetric pre-flight steps
(VMI lookup, per-VMI lock, validation) and then:

### Step 1 — single patch

`applyDetachPatch(ctx, vmi, req)` builds ONE JSONPatch that removes the
named volume from all three slices simultaneously:

- `/spec/volumes`              → rewritten without the entry, gated by `test`
- `/spec/domain/devices/disks` → rewritten without the entry, gated by `test`
- `/status/volumeStatus`       → rewritten without the entry, gated by `test`

We use `replace` (not three `remove`s) because JSONPatch `remove` is
fragile when indices shift; `test` + `replace` of the whole slice
preserves atomicity even under concurrent writers.

Detach is single-phase (unlike the two-phase attach) because the
between-step observability window would be near-zero: virt-handler's
reconcile + libvirt detach + host unmount all happen *after* spec
removal, not between two patches. There is no useful intermediate state
worth recording on the VMI.

If the volume is already absent from the VMI (someone else detached it
first, or this is a retry after our own previous success), we treat that
as idempotent success without patching.

### Step 2 — wait for libvirt

`waitDomainDiskAbsent(vmi, vol)` polls the local domain informer cache
(NOT `vmi.Status.VolumeStatus`, which is the wrong signal for this — see
the G3 fix). The cache is populated by virt-handler's domain informer
which gets fed by the QEMU notify socket; it reflects the live libvirt
XML on this node.

We wait until the disk is no longer in `domain.Spec.Devices.Disks`. The
poll has a configurable upper bound (currently a few seconds — adjust as
PoC-vs-production guidance evolves).

If the wait times out, we surface `LIBVIRT_TIMEOUT` and **leave the host
mount in place**. The reasoning is that the disk is still wired into
libvirt, so the launcher still legitimately needs the source path
visible; ripping it out would crash the VM. Operator intervention
(detach the libvirt device, then retry) is required.

### Step 3 — host-side cleanup

`UnmountBlock(vmi, vol)`:

1. Look up the launcher pod UID locally. If it's gone (launcher pod
   already cleaned up because the VMI is on its way out), return nil —
   nothing to clean up on this node.
2. Compute the device path under the launcher's `hotplug-disks` dir.
3. Call `setBlockCgroup(cgMgr, dev, false)` to remove the cgroup
   `devices.allow` rule.
4. `safepath.UnlinkAtNoFollow(devicePath)` removes the device node.

`UnmountFile(vmi, vol)`:

1. Same launcher UID lookup.
2. `virtchroot.UmountChroot(target)` undoes the bind mount in the host
   namespace; this propagates into the launcher.
3. `safepath.UnlinkAtNoFollow(target)` removes the placeholder file.

### Failure modes during detach

| Failure                                       | Code                          | Recovery                                    |
|-----------------------------------------------|-------------------------------|---------------------------------------------|
| VMI not found                                 | `VMI_NOT_FOUND`               | client treats as success                    |
| Volume not in VMI                             | `OK` (idempotent)             | n/a                                         |
| Patch fails persistently                      | `PATCH_FAILED`                | client retry                                |
| libvirt didn't release the disk in time       | `LIBVIRT_TIMEOUT`             | check VM (in-guest umount?), then retry     |
| Unmount fails (EBUSY because guest still has it open) | `UNMOUNT_FAILED`         | wait + retry; `Recover()` will retry on restart |

---

## Recover() — startup re-assertion

When virt-handler restarts, it has lost all in-process state but the
apiserver still holds the previous VMI specs and statuses. Specifically:

- `Bound` entries whose mounted-patch never landed (process killed
  between mount + step 3) need to be advanced to `MountedToPod`.
- `MountedToPod`/`Ready` entries need their host-side mounts re-asserted
  (the kubelet may have torn them down, or the launcher pod may have
  been recreated).

`Service.Recover()` walks every local VMI and, for each
`NodeLocalDevice` volume in spec:

1. Re-runs the appropriate mounter call (idempotent: `mknod` with
   existing inode is `EEXIST` which we ignore; bind-mount on an existing
   bind point is a no-op via `findmnt`-aware logic).
2. If the corresponding `volumeStatus.Phase == Bound`, drives it to
   `MountedToPod` via the same `applyAttachMountedPatch` used during
   normal attach.

`Recover()` is best-effort: failures are logged and skipped, not fatal.
The next VMI reconcile will retry.

---

## Cross-component interactions worth knowing

These bit us during PoC and are worth preserving institutional memory:

### virt-handler's existing hotplug-disk mounter

`pkg/virt-handler/hotplug-disk/mount.go::Mount` is invoked from
`vm.go::handleRunningVMI` on every reconcile tick. It iterates ALL
`vmi.Status.VolumeStatus` entries with `HotplugVolume != nil` — including
ours. For our entries `volumeStatus.HotplugVolume.AttachPodUID == ""` and
`mountHotplugVolume` short-circuits to `nil` (no work, no error). So the
existing mounter is a quiet no-op for `NodeLocalDevice` volumes. Do NOT
remove the empty `HotplugVolume: {}` from `buildAttachVolumeStatus` —
some downstream code uses it as the "this is a hotplug volume" signal
(notably `vm.go::updateVolumeStatusesFromDomain`'s phase-transition
logic, and the converter's `HotplugVolumes` map population in
`generateConverterContext`).

### virt-handler's existing hotplug-disk unmounter

`Unmount` iterates `vmi.Spec.Volumes` and skips entries that are not
`storagetypes.IsHotplugVolume(&v)`. That helper currently recognises only
PVC/DV `Hotpluggable` and `MemoryDump.Hotpluggable`; `NodeLocalDevice` is
NOT recognised, so our entries are skipped. This is the correct outcome
(we don't want the existing unmounter to tear down our mounts) but it is
currently *accidental* — if someone extends `IsHotplugVolume` to include
`NodeLocalDevice` for some other reason, this path would start
unmounting our volumes. Consider adding a "skip if NodeLocalDevice" guard
explicitly when productionising.

### virt-controller's `volumeStatusController`

The controller does NOT currently prune `NodeLocalDevice` volumeStatus
entries. We verified this empirically (entries persist across many
reconciles). If a future controller change starts pruning entries it
doesn't recognise, our `applyAttachMountedPatch` retry logic
(`errVolumeStatusStale`) would help in the immediate window but
ultimately fail.

### Live migration

Currently NOT supported. `validateAttach` rejects any VMI in a
`MigrationState != nil` until `Completed = true`. This is a deliberate
PoC limitation — making node-local hotplug live-migration-aware is its
own design problem (the destination node may not have the same host
device), out of scope for the initial cut.

### Boot disk

`validateDetach` rejects the request if the named volume is referenced
by a `Disk` with `BootOrder == 1` or is the only disk on the VMI. We
don't want to detach the disk the guest is currently booted from.

---

## SELinux deep-dive

This is the bug that took the longest to nail down during PoC, so it
gets its own section.

### The setup

- Launcher pods get a per-pod SELinux MCS pair, e.g. `s0:c86,c796`. This
  is on the launcher container's process and on every file that
  Kubernetes places into the launcher's emptyDirs.
- The launcher's QEMU process inherits the same MCS.
- `vmi.Status.SelinuxContext` is populated by virt-handler with the
  launcher's full SELinux context once the pod is up.

### Block path — works automatically

`MountBlock` uses `safepath.MknodAtNoFollow` to create a fresh device
inode inside the launcher's `hotplug-disks` emptyDir. The new inode
inherits the directory's SELinux MCS (the launcher's MCS), so QEMU can
open it. No relabel needed.

### File path — needs explicit relabel

`MountFile` does `mount -o bind <host file> <launcher dir>/<vol>.img`.
Bind mounts share the source inode, including its SELinux xattr. So the
file inside the launcher pod retains the host file's MCS — typically
something unrelated to the launcher's MCS. QEMU cannot read it. The
failure is silent: `checkIfDiskReadyToUse`'s `os.OpenFile(O_RDWR)`
returns EACCES, the function returns `(false, nil)`, the loop in
`syncDisks` `continue`s, no log line, no error. Symptoms:

- `vmi.Status.VolumeStatus[<vol>].Phase` stays at `MountedToPod`.
- `Target` stays empty.
- `qemuDomainGetBlockInfo: invalid path … not assigned to domain`
  appears in launcher logs (this is the *expansion* loop running on
  the converter-built disk that was never attached to libvirt).

We fix this by calling `selinux.RelabelFilesUnprivileged(true, target)`
right after the bind mount succeeds. That sets the inode's label to
`system_u:object_r:container_file_t:s0` (no MCS categories). `s0` with
no categories is dominated by every per-pod MCS, so any launcher pod can
read it.

This **mutates the underlying host file's SELinux label** (because the
bind mount and the source share the inode). For a host file that is
already an "external" disk image meant for VM consumption, this is
acceptable: container_file_t with no MCS is the standard label for
container-shared files. If the host file is also used by some other
process that requires a more restrictive label, the operator must place
a correctly-labelled copy at a dedicated path before calling
`AttachDevice`.

`continueOnError=true` in the relabel call keeps us moving when SELinux
is disabled or in permissive mode — the relabel command can fail in
obscure ways in those configurations but the bind mount is already valid
and the launcher will be able to open the file.

### Manual fallback for one-off testing

If you need to validate the rest of the flow without relying on the
relabel (e.g. on a cluster where SELinux policy forbids
`container_file_t:s0`), see `tests/manual-attach-pod.yaml.example` (TODO)
for a relay-pod recipe that mirrors the existing PVC attach-pod pattern.

---

## Concurrency model

- Per-VMI mutex (`vmiLock`) serialises all Attach/Detach calls for a
  single VMI. Concurrent calls for different VMIs proceed in parallel.
- The patch helpers (`applyAttachIntentPatch`,
  `applyAttachMountedPatch`, `applyDetachPatch`) wrap their apiserver
  call in `retry.OnError(retry.DefaultRetry, isPatchRetriable, …)`.
  `isPatchRetriable` matches:
    - `apierrors.IsConflict` (409 — `resourceVersion` raced)
    - `apierrors.IsInvalid`  (422 — JSONPatch `test` op missed)
    - `errVolumeStatusStale` (our own sentinel for cached-VMI lag in
      `applyAttachMountedPatch`)
- The mount helpers are NOT individually idempotent at the syscall
  layer (a second `mknod` with the same name returns `EEXIST`) but the
  wrappers tolerate `EEXIST` and `ENOENT` so calling Attach twice in a
  row is safe.

---

## Reading list

If you're modifying this package, also read:

- `pkg/virt-launcher/virtwrap/manager.go::syncDisks` — the consumer of
  our spec changes. `getAttachedDisks` + `checkIfDiskReadyToUse` is
  where attach actually happens; understanding the silent-skip behaviour
  is essential for diagnosing "stuck at MountedToPod" reports.
- `pkg/virt-launcher/virtwrap/converter/converter.go::Convert_v1_Hotplug_Volume_To_api_Disk`
  — our G2 arm lives here; without it the converter rejects
  `NodeLocalDevice` volumes with "unsupported source".
- `pkg/storage/admitters/storagehotplug.go::verifyHotplugVolumes` — our
  G1 fix admits `NodeLocalDevice` as a valid source. Without G1 the
  admission webhook rejects our intent patch with "PVC or DV is the
  only source supported for hotplug".
- `pkg/virt-handler/vm.go::updateHotplugVolumeStatus` — the engine that
  drives `Bound → MountedToPod` (when virt-handler races us) and
  `MountedToPod → Ready` (when libvirt finishes attaching). Read
  alongside `updateVolumeStatusesFromDomain` to see how `Target` is
  populated.

---

## Glossary

- **launcher pod / virt-launcher**: the pod that runs QEMU for the VMI.
  One per VMI (sometimes two during migration).
- **`hotplug-disks` emptyDir**: a per-launcher-pod emptyDir at
  `/var/run/kubevirt/hotplug-disks/` (in the container view), backed by
  `/var/lib/kubelet/pods/<launcher-uid>/volumes/kubernetes.io~empty-dir/hotplug-disks/`
  on the host. Mounted with `HostToContainer` propagation.
- **virt-chroot**: a small KubeVirt-shipped helper that runs `nsenter --mount=/proc/1/ns/mnt`
  before the actual command, used to perform mount/umount/relabel in
  the host's mount namespace from inside virt-handler's container.
- **MCS / SELinux level**: the `s0:cN,cM` portion of an SELinux context.
  Kubernetes assigns a unique MCS pair per launcher pod for VMI isolation.
- **JSONPatch `test` op**: `{"op":"test","path":"…","value":<v>}`. The
  apiserver returns 422 Invalid if the value at `path` is not equal to
  `<v>`. We use this to gate every modification with an
  optimistic-concurrency check that targets only the slice we are about
  to mutate, leaving the rest of the VMI free to change concurrently.
