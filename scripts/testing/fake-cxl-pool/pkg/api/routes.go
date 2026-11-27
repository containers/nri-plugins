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

package api

import "net/url"

const (
	// Prefix is the path prefix of all API v1 routes.
	Prefix = "/api/v1"
	// DefaultListen is the default listen address of the server.
	DefaultListen = "127.0.0.1:9909"
	// DefaultServerURL is the default server URL for clients in VMs:
	// the slirp gateway address reaches the host loopback.
	DefaultServerURL = "http://192.168.76.2:9909"
	// EnvServer is the environment variable that overrides DefaultServerURL.
	EnvServer = "FAKE_CXL_POOL_SERVER"
)

// Route patterns (net/http ServeMux, Go 1.22+ syntax).
const (
	RouteStatus        = "GET " + Prefix + "/status"
	RouteHosts         = "GET " + Prefix + "/hosts"
	RouteHostsResolve  = "GET " + Prefix + "/hosts/resolve"
	RouteHost          = "GET " + Prefix + "/hosts/{name}"
	RouteDevices       = "GET " + Prefix + "/devices"
	RouteDevicesCreate = "POST " + Prefix + "/devices"
	RouteDevice        = "GET " + Prefix + "/devices/{name}"
	RouteDeviceDelete  = "DELETE " + Prefix + "/devices/{name}"
	RouteDeviceAttach  = "POST " + Prefix + "/devices/{name}/attachments"
	RouteDeviceDetach  = "DELETE " + Prefix + "/devices/{name}/attachments/{host}"
	RouteAttachments   = "GET " + Prefix + "/attachments"
	RouteEvents        = "GET " + Prefix + "/events"
)

// Paths for clients.

func PathStatus() string       { return Prefix + "/status" }
func PathHosts() string        { return Prefix + "/hosts" }
func PathHostsResolve() string { return Prefix + "/hosts/resolve" }
func PathHost(n string) string { return Prefix + "/hosts/" + url.PathEscape(n) }
func PathDevices() string      { return Prefix + "/devices" }
func PathDevice(n string) string {
	return Prefix + "/devices/" + url.PathEscape(n)
}
func PathDeviceAttachments(dev string) string {
	return PathDevice(dev) + "/attachments"
}
func PathDeviceAttachment(dev, host string) string {
	return PathDeviceAttachments(dev) + "/" + url.PathEscape(host)
}
func PathAttachments() string { return Prefix + "/attachments" }
func PathEvents() string      { return Prefix + "/events" }
