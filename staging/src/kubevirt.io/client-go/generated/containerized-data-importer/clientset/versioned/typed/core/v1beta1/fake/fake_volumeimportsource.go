/*
Copyright 2023 The KubeVirt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
	v1beta1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
)

// FakeVolumeImportSources implements VolumeImportSourceInterface
type FakeVolumeImportSources struct {
	Fake *FakeCdiV1beta1
	ns   string
}

var volumeimportsourcesResource = schema.GroupVersionResource{Group: "cdi.kubevirt.io", Version: "v1beta1", Resource: "volumeimportsources"}

var volumeimportsourcesKind = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "VolumeImportSource"}

// Get takes name of the volumeImportSource, and returns the corresponding volumeImportSource object, and an error if there is any.
func (c *FakeVolumeImportSources) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1beta1.VolumeImportSource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(volumeimportsourcesResource, c.ns, name), &v1beta1.VolumeImportSource{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.VolumeImportSource), err
}

// List takes label and field selectors, and returns the list of VolumeImportSources that match those selectors.
func (c *FakeVolumeImportSources) List(ctx context.Context, opts v1.ListOptions) (result *v1beta1.VolumeImportSourceList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(volumeimportsourcesResource, volumeimportsourcesKind, c.ns, opts), &v1beta1.VolumeImportSourceList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1beta1.VolumeImportSourceList{ListMeta: obj.(*v1beta1.VolumeImportSourceList).ListMeta}
	for _, item := range obj.(*v1beta1.VolumeImportSourceList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested volumeImportSources.
func (c *FakeVolumeImportSources) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(volumeimportsourcesResource, c.ns, opts))

}

// Create takes the representation of a volumeImportSource and creates it.  Returns the server's representation of the volumeImportSource, and an error, if there is any.
func (c *FakeVolumeImportSources) Create(ctx context.Context, volumeImportSource *v1beta1.VolumeImportSource, opts v1.CreateOptions) (result *v1beta1.VolumeImportSource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(volumeimportsourcesResource, c.ns, volumeImportSource), &v1beta1.VolumeImportSource{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.VolumeImportSource), err
}

// Update takes the representation of a volumeImportSource and updates it. Returns the server's representation of the volumeImportSource, and an error, if there is any.
func (c *FakeVolumeImportSources) Update(ctx context.Context, volumeImportSource *v1beta1.VolumeImportSource, opts v1.UpdateOptions) (result *v1beta1.VolumeImportSource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(volumeimportsourcesResource, c.ns, volumeImportSource), &v1beta1.VolumeImportSource{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.VolumeImportSource), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeVolumeImportSources) UpdateStatus(ctx context.Context, volumeImportSource *v1beta1.VolumeImportSource, opts v1.UpdateOptions) (*v1beta1.VolumeImportSource, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(volumeimportsourcesResource, "status", c.ns, volumeImportSource), &v1beta1.VolumeImportSource{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.VolumeImportSource), err
}

// Delete takes name of the volumeImportSource and deletes it. Returns an error if one occurs.
func (c *FakeVolumeImportSources) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteAction(volumeimportsourcesResource, c.ns, name), &v1beta1.VolumeImportSource{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeVolumeImportSources) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(volumeimportsourcesResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &v1beta1.VolumeImportSourceList{})
	return err
}

// Patch applies the patch and returns the patched volumeImportSource.
func (c *FakeVolumeImportSources) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1beta1.VolumeImportSource, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(volumeimportsourcesResource, c.ns, name, pt, data, subresources...), &v1beta1.VolumeImportSource{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1beta1.VolumeImportSource), err
}