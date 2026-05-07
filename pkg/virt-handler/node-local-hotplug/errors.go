/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package nodelocalhotplug

import (
	"errors"
	"fmt"

	apiv1 "kubevirt.io/kubevirt/pkg/virt-handler/node-local-hotplug/v1"
)

// codedError carries a NodeLocalHotplugErrorCode alongside a wrapped
// error. validator.go produces these for the cases it can classify
// precisely (UID mismatch, VMI not running, host path bad, etc.); the
// service layer extracts the code via codeOf when building gRPC
// responses, falling back to the per-call-site default if the error is
// not coded.
type codedError struct {
	code apiv1.NodeLocalHotplugErrorCode
	err  error
}

// newCodedError wraps err with the supplied code. The returned value
// formats with the same string as the underlying error, so log lines
// that already include the message stay readable; callers that want
// the code use codeOf.
func newCodedError(code apiv1.NodeLocalHotplugErrorCode, err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: code, err: err}
}

// codedErrorf is the fmt.Errorf-flavoured constructor.
func codedErrorf(code apiv1.NodeLocalHotplugErrorCode, format string, a ...any) error {
	return &codedError{code: code, err: fmt.Errorf(format, a...)}
}

// Error implements the error interface.
func (e *codedError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

// Unwrap exposes the wrapped error so errors.Is / errors.As keep working
// across boundaries (e.g. retry classifiers in the service layer).
func (e *codedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// codeOf walks the error chain and returns the first NodeLocalHotplug
// error code it finds, or fallback if the chain has no codedError.
func codeOf(err error, fallback apiv1.NodeLocalHotplugErrorCode) apiv1.NodeLocalHotplugErrorCode {
	if err == nil {
		return apiv1.NodeLocalHotplugErrorCode_NODE_LOCAL_HOTPLUG_OK
	}
	var ce *codedError
	if errors.As(err, &ce) && ce != nil {
		return ce.code
	}
	return fallback
}
