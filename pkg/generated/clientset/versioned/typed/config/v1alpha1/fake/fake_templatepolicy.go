// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"

	v1alpha1 "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"
)

// FakeTemplatePolicies implements TemplatePolicyInterface
type FakeTemplatePolicies struct {
	Fake *FakeConfigV1alpha1
	ns   string
}

var templatepoliciesResource = v1alpha1.SchemeGroupVersion.WithResource("templatepolicies")

var templatepoliciesKind = v1alpha1.SchemeGroupVersion.WithKind("TemplatePolicy")

// Get takes name of the templatePolicy, and returns the corresponding templatePolicy object, and an error if there is any.
func (c *FakeTemplatePolicies) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.TemplatePolicy, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewGetAction(templatepoliciesResource, c.ns, name), &v1alpha1.TemplatePolicy{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.TemplatePolicy), err
}

// List takes label and field selectors, and returns the list of TemplatePolicies that match those selectors.
func (c *FakeTemplatePolicies) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.TemplatePolicyList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewListAction(templatepoliciesResource, templatepoliciesKind, c.ns, opts), &v1alpha1.TemplatePolicyList{})

	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1alpha1.TemplatePolicyList{ListMeta: obj.(*v1alpha1.TemplatePolicyList).ListMeta}
	for _, item := range obj.(*v1alpha1.TemplatePolicyList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested templatePolicies.
func (c *FakeTemplatePolicies) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchAction(templatepoliciesResource, c.ns, opts))

}

// Create takes the representation of a templatePolicy and creates it.  Returns the server's representation of the templatePolicy, and an error, if there is any.
func (c *FakeTemplatePolicies) Create(ctx context.Context, templatePolicy *v1alpha1.TemplatePolicy, opts v1.CreateOptions) (result *v1alpha1.TemplatePolicy, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewCreateAction(templatepoliciesResource, c.ns, templatePolicy), &v1alpha1.TemplatePolicy{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.TemplatePolicy), err
}

// Update takes the representation of a templatePolicy and updates it. Returns the server's representation of the templatePolicy, and an error, if there is any.
func (c *FakeTemplatePolicies) Update(ctx context.Context, templatePolicy *v1alpha1.TemplatePolicy, opts v1.UpdateOptions) (result *v1alpha1.TemplatePolicy, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateAction(templatepoliciesResource, c.ns, templatePolicy), &v1alpha1.TemplatePolicy{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.TemplatePolicy), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeTemplatePolicies) UpdateStatus(ctx context.Context, templatePolicy *v1alpha1.TemplatePolicy, opts v1.UpdateOptions) (*v1alpha1.TemplatePolicy, error) {
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceAction(templatepoliciesResource, "status", c.ns, templatePolicy), &v1alpha1.TemplatePolicy{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.TemplatePolicy), err
}

// Delete takes name of the templatePolicy and deletes it. Returns an error if one occurs.
func (c *FakeTemplatePolicies) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteActionWithOptions(templatepoliciesResource, c.ns, name, opts), &v1alpha1.TemplatePolicy{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeTemplatePolicies) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionAction(templatepoliciesResource, c.ns, listOpts)

	_, err := c.Fake.Invokes(action, &v1alpha1.TemplatePolicyList{})
	return err
}

// Patch applies the patch and returns the patched templatePolicy.
func (c *FakeTemplatePolicies) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.TemplatePolicy, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceAction(templatepoliciesResource, c.ns, name, pt, data, subresources...), &v1alpha1.TemplatePolicy{})

	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.TemplatePolicy), err
}
