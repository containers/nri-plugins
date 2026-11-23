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

package dra

import (
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/containers/nri-plugins/pkg/cxl/device"
)

// TestConsumedShares: the shares guard of shared devices and the use that
// the Prepare logs.
func TestConsumedShares(t *testing.T) {
	result := func(shares string) *resourcev1.DeviceRequestAllocationResult {
		r := &resourcev1.DeviceRequestAllocationResult{}
		if shares != "" {
			r.ConsumedCapacity = map[resourcev1.QualifiedName]resource.Quantity{device.CapacityShares: resource.MustParse(shares)}
		}
		return r
	}
	for _, tc := range []struct {
		shares string
		want   int64
		use    string
		err    string
	}{
		{shares: "1M", want: 1000000, use: "exclusive"},
		{shares: "1", want: 1, use: "1 share"},
		{shares: "3", want: 3, use: "3 shares"},
		{shares: "0", err: `memdev 0xc100e2e1: consumed shares "0" is less than 1: request shares: 1 (a share) or no shares (exclusive)`},
		{shares: "0.5", err: `memdev 0xc100e2e1: consumed shares "500m" is less than 1: request shares: 1 (a share) or no shares (exclusive)`},
		{shares: "", err: `memdev 0xc100e2e1: consumed shares "" is less than 1: request shares: 1 (a share) or no shares (exclusive)`},
		{shares: "-1", err: `memdev 0xc100e2e1: consumed shares "-1" is less than 1: request shares: 1 (a share) or no shares (exclusive)`},
	} {
		got, err := consumedShares(0xc100e2e1, result(tc.shares))
		switch {
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Errorf("%q: error %v, expected %q", tc.shares, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want || sharesText(got, device.SharesUnlimited) != tc.use):
			t.Errorf("%q: %d %v, expected %d %s", tc.shares, got, err, tc.want, tc.use)
		}
	}
}
