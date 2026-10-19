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

package e2eish

import (
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const draTestDriver = "topology-aware.nri.io"

// draTestClass is the DeviceClass of the test driver.
func draTestClass() *resourceapi.DeviceClass {
	class := &resourceapi.DeviceClass{}
	class.Name = draTestDriver
	class.Spec.Selectors = []resourceapi.DeviceSelector{{
		CEL: &resourceapi.CELDeviceSelector{
			Expression: `device.driver == "` + draTestDriver + `"`,
		},
	}}
	return class
}

// draTestTemplate is a ResourceClaimTemplate of one request per name, each
// for cpus CPUs of the NUMA node, as test42-dra-devices writes them. The
// allocation mode and the count are left out, as the e2e test leaves them to
// the API server.
func draTestTemplate(name string, cpus int, nodes map[string]int) *resourceapi.ResourceClaimTemplate {
	tmpl := &resourceapi.ResourceClaimTemplate{}
	tmpl.Name = name
	for _, req := range slices.Sorted(maps.Keys(nodes)) {
		tmpl.Spec.Spec.Devices.Requests = append(tmpl.Spec.Spec.Devices.Requests,
			resourceapi.DeviceRequest{
				Name: req,
				Exactly: &resourceapi.ExactDeviceRequest{
					DeviceClassName: draTestDriver,
					Capacity: &resourceapi.CapacityRequirements{
						Requests: map[resourceapi.QualifiedName]resource.Quantity{
							"dra.cpu/cpu": *resource.NewQuantity(int64(cpus), resource.DecimalSI),
						},
					},
					Selectors: []resourceapi.DeviceSelector{{
						CEL: &resourceapi.CELDeviceSelector{
							Expression: `device.attributes["resource.kubernetes.io"].numaNode == ` +
								strconv.Itoa(nodes[req]),
						},
					}},
				},
			})
	}
	return tmpl
}

// draTestDevices is one device per NUMA node, each offering cpus CPUs to any
// number of claims at once, as the topology-aware policy publishes them.
func draTestDevices(nodes, cpus int) []resourceapi.Device {
	one := resource.NewQuantity(1, resource.DecimalSI)
	var devices []resourceapi.Device
	for id := range nodes {
		devices = append(devices, resourceapi.Device{
			Name:                     "cpudevnuma00" + strconv.Itoa(id),
			AllowMultipleAllocations: ptr.To(true),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"resource.kubernetes.io/numaNode": {IntValue: ptr.To(int64(id))},
			},
			Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				"dra.cpu/cpu": {
					Value: *resource.NewQuantity(int64(cpus), resource.DecimalSI),
					RequestPolicy: &resourceapi.CapacityRequestPolicy{
						Default:    one,
						ValidRange: &resourceapi.CapacityRequestPolicyRange{Min: one, Step: one},
					},
				},
			},
		})
	}
	return devices
}

// draTestPod is a pod with one claim of a template, held whole by each of
// the named containers.
func draTestPod(name, template string, ctrs ...string) *corev1.Pod {
	pod := &corev1.Pod{}
	pod.Name = name
	pod.Spec.ResourceClaims = []corev1.PodResourceClaim{
		{Name: "cpus", ResourceClaimTemplateName: &template},
	}
	for _, c := range ctrs {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name:      c,
			Resources: corev1.ResourceRequirements{Claims: []corev1.ResourceClaim{{Name: "cpus"}}},
		})
	}
	return pod
}

// draTestCluster is a cluster with the DeviceClass of the test driver.
func draTestCluster() *draCluster {
	d := newDRACluster(draTestDriver)
	d.addClass(draTestClass())
	return d
}

// allocatePod creates the claims of a pod and allocates them.
func allocatePod(t *testing.T, d *draCluster, devices []resourceapi.Device, pod *corev1.Pod) ([]podClaim, bool) {
	t.Helper()
	claims, err := d.podClaims("default", pod)
	require.NoError(t, err)
	fits, err := d.allocate(devices, claims)
	require.NoError(t, err)
	if fits {
		d.own("default", pod.Name, claims)
	}
	return claims, fits
}

// A claim of a template is allocated the device its selector picks, and the
// CPUs it asked for.
func TestDRAAllocateSelectsNode(t *testing.T) {
	d := draTestCluster()
	d.addTemplate("default", draTestTemplate("pod0", 2, map[string]int{"cpus": 2}))

	claims, fits := allocatePod(t, d, draTestDevices(4, 4), draTestPod("pod0", "pod0", "pod0c0"))
	require.True(t, fits, "the claim fits")
	require.Len(t, claims, 1)

	results := d.results(claims[0].claim)
	require.Len(t, results, 1, "one result for the one request")
	require.Equal(t, "cpudevnuma002", results[0].Device, "the device of the node the selector names")
	cpus := results[0].ConsumedCapacity["dra.cpu/cpu"]
	require.Equal(t, "2", cpus.String())
}

// A claim asking for more CPUs than its node offers does not fit, and the
// pod of it is not started.
func TestDRAAllocateDoesNotFit(t *testing.T) {
	d := draTestCluster()
	d.addTemplate("default", draTestTemplate("pod0", 5, map[string]int{"cpus": 0}))

	_, fits := allocatePod(t, d, draTestDevices(4, 4), draTestPod("pod0", "pod0", "pod0c0"))
	require.False(t, fits, "a claim for 5 of the 4 CPUs of a node does not fit")
}

// The claims allocated so far consume the capacity of their device, so a
// claim which the node has no CPUs left for does not fit until the pod
// holding them is deleted.
func TestDRAAllocateAccountsForEarlierClaims(t *testing.T) {
	d := draTestCluster()
	devices := draTestDevices(4, 4)
	for _, name := range []string{"pod0", "pod1", "pod2"} {
		d.addTemplate("default", draTestTemplate(name, 2, map[string]int{"cpus": 1}))
	}

	_, fits := allocatePod(t, d, devices, draTestPod("pod0", "pod0", "pod0c0"))
	require.True(t, fits, "the first 2 of the 4 CPUs of node 1")
	_, fits = allocatePod(t, d, devices, draTestPod("pod1", "pod1", "pod1c0"))
	require.True(t, fits, "the other 2 CPUs of node 1")
	_, fits = allocatePod(t, d, devices, draTestPod("pod2", "pod2", "pod2c0"))
	require.False(t, fits, "no CPUs of node 1 are left")

	require.Len(t, d.release("default", "pod0"), 1, "deleting pod0 releases its claim")
	_, fits = allocatePod(t, d, devices, draTestPod("pod2", "pod2", "pod2c0"))
	require.True(t, fits, "the CPUs pod0 released")
}

// Every container holding a claim whole gets the CDI device of each of its
// results, and a container naming one request only that request's.
func TestDRAContainerDevices(t *testing.T) {
	d := draTestCluster()
	d.addTemplate("default", draTestTemplate("pod8", 1, map[string]int{"first": 0, "second": 2}))

	pod := draTestPod("pod8", "pod8", "pod8c2")
	pod.Spec.Containers = append([]corev1.Container{
		{
			Name: "pod8c0",
			Resources: corev1.ResourceRequirements{
				Claims: []corev1.ResourceClaim{{Name: "cpus", Request: "first"}},
			},
		},
		{
			Name: "pod8c1",
			Resources: corev1.ResourceRequirements{
				Claims: []corev1.ResourceClaim{{Name: "cpus", Request: "second"}},
			},
		},
	}, pod.Spec.Containers...)

	claims, fits := allocatePod(t, d, draTestDevices(4, 4), pod)
	require.True(t, fits)
	require.Len(t, d.results(claims[0].claim), 2, "one result per request")

	uid := claims[0].claim.UID
	require.Equal(t, map[string][]string{
		"pod8c0": {draTestDriver + "/device=claim-" + string(uid) + "-0"},
		"pod8c1": {draTestDriver + "/device=claim-" + string(uid) + "-1"},
		"pod8c2": {
			draTestDriver + "/device=claim-" + string(uid) + "-0",
			draTestDriver + "/device=claim-" + string(uid) + "-1",
		},
	}, d.containerDevices(pod, claims))
}

// A claim of the same pod and claim name gets the same UID every time, so
// that a regenerated test does not churn.
func TestDRAClaimUIDIsDerived(t *testing.T) {
	d := draTestCluster()
	d.addTemplate("default", draTestTemplate("pod0", 1, map[string]int{"cpus": 0}))

	first, err := d.podClaims("default", draTestPod("pod0", "pod0", "pod0c0"))
	require.NoError(t, err)
	second, err := d.podClaims("default", draTestPod("pod0", "pod0", "pod0c0"))
	require.NoError(t, err)
	require.Equal(t, first[0].claim.UID, second[0].claim.UID)
}

// A pod naming a template which was never created stops the replay rather
// than silently holding no claim.
func TestDRAUnknownTemplate(t *testing.T) {
	_, err := draTestCluster().podClaims("default", draTestPod("pod0", "pod0", "pod0c0"))
	require.ErrorContains(t, err, "unknown template default/pod0")
}

// The allocator needs the allocation mode and the count the API server
// defaults, which the log of an e2e test does not have.
func TestDRADefaultClaimSpec(t *testing.T) {
	spec := &draTestTemplate("pod0", 2, map[string]int{"cpus": 0}).Spec.Spec
	require.Empty(t, spec.Devices.Requests[0].Exactly.AllocationMode, "the e2e test sets neither")
	require.Zero(t, spec.Devices.Requests[0].Exactly.Count)

	defaultClaimSpec(spec)
	require.Equal(t, resourceapi.DeviceAllocationModeExactCount,
		spec.Devices.Requests[0].Exactly.AllocationMode)
	require.Equal(t, int64(1), spec.Devices.Requests[0].Exactly.Count)
}

// The YAML the generator writes for a DRA object parses back into the same
// object, so a generated test creates what the e2e test created.
func TestDRAObjectYAMLRoundTrip(t *testing.T) {
	g := newGenTest(t)
	g.plugin.DRADriver = draTestDriver

	for _, tc := range []struct {
		name string
		obj  any
		into func() any
	}{
		{"DeviceClass", draTestClass(), func() any { return &resourceapi.DeviceClass{} }},
		{
			"ResourceClaimTemplate",
			draTestTemplate("pod8", 1, map[string]int{"first": 0, "second": 2}),
			func() any { return &resourceapi.ResourceClaimTemplate{} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &record{Op: "create", Namespace: "default", Object: jsonOf(t, tc.obj)}
			y := g.draObject(r, tc.into())
			require.NotEmpty(t, y, "the object is written")
			require.Empty(t, g.stopped)

			back := tc.into()
			require.NoError(t, yaml.UnmarshalStrict([]byte(y), back), "the YAML parses strictly")
			// Compared as JSON: a resource.Quantity caches the string it
			// was parsed from, which a built one has not got.
			require.JSONEq(t, string(jsonOf(t, tc.obj)), string(jsonOf(t, back)),
				"the YAML holds the object itself")
		})
	}
}

// A field of a DRA object the replay does not know stops the generation,
// rather than being dropped from a test which would then assert less.
func TestDRAObjectUnknownFieldStops(t *testing.T) {
	g := newGenTest(t)
	r := &record{
		Op:        "create",
		Namespace: "default",
		Object:    []byte(`{"kind":"DeviceClass","metadata":{"name":"x"},"spec":{"whatIsThis":1}}`),
	}

	require.Empty(t, g.draObject(r, &resourceapi.DeviceClass{}))
	require.Contains(t, g.stopped, "whatIsThis")
}
