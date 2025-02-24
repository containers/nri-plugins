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

// FakeBalloonsPolicies implements BalloonsPolicyInterface
type FakeBalloonsPolicies struct {
	Fake *FakeConfigV1alpha1
	ns   string
}

var balloonspoliciesResource = v1alpha1.SchemeGroupVersion.WithResource("balloonspolicies")

var balloonspoliciesKind = v1alpha1.SchemeGroupVersion.WithKind("BalloonsPolicy")

// Get takes name of the balloonsPolicy, and returns the corresponding balloonsPolicy object, and an error if there is any.
func (c *FakeBalloonsPolicies) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.BalloonsPolicy, err error) {
	emptyResult := &v1alpha1.BalloonsPolicy{}
	obj, err := c.Fake.
		Invokes(testing.NewGetActionWithOptions(balloonspoliciesResource, c.ns, name, options), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1alpha1.BalloonsPolicy), err
}

// List takes label and field selectors, and returns the list of BalloonsPolicies that match those selectors.
func (c *FakeBalloonsPolicies) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.BalloonsPolicyList, err error) {
	emptyResult := &v1alpha1.BalloonsPolicyList{}
	obj, err := c.Fake.
		Invokes(testing.NewListActionWithOptions(balloonspoliciesResource, balloonspoliciesKind, c.ns, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1alpha1.BalloonsPolicyList{ListMeta: obj.(*v1alpha1.BalloonsPolicyList).ListMeta}
	for _, item := range obj.(*v1alpha1.BalloonsPolicyList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested balloonsPolicies.
func (c *FakeBalloonsPolicies) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewWatchActionWithOptions(balloonspoliciesResource, c.ns, opts))

}

// Create takes the representation of a balloonsPolicy and creates it.  Returns the server's representation of the balloonsPolicy, and an error, if there is any.
func (c *FakeBalloonsPolicies) Create(ctx context.Context, balloonsPolicy *v1alpha1.BalloonsPolicy, opts v1.CreateOptions) (result *v1alpha1.BalloonsPolicy, err error) {
	emptyResult := &v1alpha1.BalloonsPolicy{}
	obj, err := c.Fake.
		Invokes(testing.NewCreateActionWithOptions(balloonspoliciesResource, c.ns, balloonsPolicy, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1alpha1.BalloonsPolicy), err
}

// Update takes the representation of a balloonsPolicy and updates it. Returns the server's representation of the balloonsPolicy, and an error, if there is any.
func (c *FakeBalloonsPolicies) Update(ctx context.Context, balloonsPolicy *v1alpha1.BalloonsPolicy, opts v1.UpdateOptions) (result *v1alpha1.BalloonsPolicy, err error) {
	emptyResult := &v1alpha1.BalloonsPolicy{}
	obj, err := c.Fake.
		Invokes(testing.NewUpdateActionWithOptions(balloonspoliciesResource, c.ns, balloonsPolicy, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1alpha1.BalloonsPolicy), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeBalloonsPolicies) UpdateStatus(ctx context.Context, balloonsPolicy *v1alpha1.BalloonsPolicy, opts v1.UpdateOptions) (result *v1alpha1.BalloonsPolicy, err error) {
	emptyResult := &v1alpha1.BalloonsPolicy{}
	obj, err := c.Fake.
		Invokes(testing.NewUpdateSubresourceActionWithOptions(balloonspoliciesResource, "status", c.ns, balloonsPolicy, opts), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1alpha1.BalloonsPolicy), err
}

// Delete takes name of the balloonsPolicy and deletes it. Returns an error if one occurs.
func (c *FakeBalloonsPolicies) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewDeleteActionWithOptions(balloonspoliciesResource, c.ns, name, opts), &v1alpha1.BalloonsPolicy{})

	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeBalloonsPolicies) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewDeleteCollectionActionWithOptions(balloonspoliciesResource, c.ns, opts, listOpts)

	_, err := c.Fake.Invokes(action, &v1alpha1.BalloonsPolicyList{})
	return err
}

// Patch applies the patch and returns the patched balloonsPolicy.
func (c *FakeBalloonsPolicies) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.BalloonsPolicy, err error) {
	emptyResult := &v1alpha1.BalloonsPolicy{}
	obj, err := c.Fake.
		Invokes(testing.NewPatchSubresourceActionWithOptions(balloonspoliciesResource, c.ns, name, pt, data, opts, subresources...), emptyResult)

	if obj == nil {
		return emptyResult, err
	}
	return obj.(*v1alpha1.BalloonsPolicy), err
}
