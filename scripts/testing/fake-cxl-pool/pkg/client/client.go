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

// Package client is the Go client library of the fake-cxl-pool REST API.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// Client talks to a fake-cxl-pool server.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// DefaultURL returns $FAKE_CXL_POOL_SERVER, or api.DefaultServerURL.
func DefaultURL() string {
	if u := os.Getenv(api.EnvServer); u != "" {
		return u
	}
	return api.DefaultServerURL
}

// New returns a client for the server URL ("" means DefaultURL()).
func New(serverURL string) *Client {
	if serverURL == "" {
		serverURL = DefaultURL()
	}
	if !strings.Contains(serverURL, "://") {
		serverURL = "http://" + serverURL
	}
	return &Client{
		BaseURL: strings.TrimRight(serverURL, "/"),
		// The proxy is never wanted: the server is on the VM host. No
		// overall timeout: detach waits up to its timeout on the server.
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		}},
	}
}

// do sends a request and decodes a JSON response into out. Non-2xx
// responses are returned as *api.Error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("cannot reach fake-cxl-pool server: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &api.Error{}
		if json.Unmarshal(data, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
			if e.Message == "" {
				e.Message = resp.Status
			}
		}
		if e.Code == "" {
			e.Code = api.CodeForHTTPStatus(resp.StatusCode)
		}
		return resp.StatusCode, e
	}
	if out != nil && len(data) > 0 && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("invalid response from %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// ErrorObject decodes the object attached to an *api.Error into v.
func ErrorObject(err error, v any) bool {
	e, ok := errors.AsType[*api.Error](err)
	if !ok || e.Object == nil {
		return false
	}
	b, merr := json.Marshal(e.Object)
	if merr != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// Status returns the server status.
func (c *Client) Status(ctx context.Context) (*api.Status, error) {
	var st api.Status
	_, err := c.do(ctx, http.MethodGet, api.PathStatus(), nil, nil, &st)
	return &st, err
}

// Hosts lists hosts.
func (c *Client) Hosts(ctx context.Context) ([]api.Host, error) {
	var hs []api.Host
	_, err := c.do(ctx, http.MethodGet, api.PathHosts(), nil, nil, &hs)
	return hs, err
}

// Host returns a host by name or uuid.
func (c *Client) Host(ctx context.Context, name string) (*api.Host, error) {
	var h api.Host
	_, err := c.do(ctx, http.MethodGet, api.PathHost(name), nil, nil, &h)
	return &h, err
}

// Resolve finds the host of a hostname and/or SMBIOS system uuid.
func (c *Client) Resolve(ctx context.Context, hostname, uuid string) (*api.Host, error) {
	q := url.Values{}
	if hostname != "" {
		q.Set("hostname", hostname)
	}
	if uuid != "" {
		q.Set("uuid", uuid)
	}
	var h api.Host
	_, err := c.do(ctx, http.MethodGet, api.PathHostsResolve(), q, nil, &h)
	return &h, err
}

// UUIDPaths are tried in order to find the system uuid of this machine:
// the SMBIOS system uuid (needs root, and a kernel with CONFIG_DMI), then
// the machine id (the e2e VMs set it to the qemu -uuid without dashes; the
// server compares uuids without dashes).
var UUIDPaths = []string{"/sys/class/dmi/id/product_uuid", "/etc/machine-id"}

// SelfIdentity returns the hostname and the system uuid ("" if none can be
// read) of this machine.
func SelfIdentity() (hostname, uuid string) {
	hostname, _ = os.Hostname()
	for _, p := range UUIDPaths {
		if b, err := os.ReadFile(p); err == nil {
			if u := strings.ToLower(strings.TrimSpace(string(b))); u != "" {
				return hostname, u
			}
		}
	}
	return hostname, ""
}

// Self resolves the host this client runs in.
func (c *Client) Self(ctx context.Context) (*api.Host, error) {
	hostname, uuid := SelfIdentity()
	return c.Resolve(ctx, hostname, uuid)
}

// Devices lists the devices of a scope ("" for all).
func (c *Client) Devices(ctx context.Context, scope string) ([]api.Device, error) {
	q := url.Values{}
	if scope != "" {
		q.Set("scope", scope)
	}
	var ds []api.Device
	_, err := c.do(ctx, http.MethodGet, api.PathDevices(), q, nil, &ds)
	return ds, err
}

// Device returns a device.
func (c *Client) Device(ctx context.Context, name string) (*api.Device, error) {
	var d api.Device
	_, err := c.do(ctx, http.MethodGet, api.PathDevice(name), nil, nil, &d)
	return &d, err
}

// CreateDevice creates a pool device.
func (c *Client) CreateDevice(ctx context.Context, req api.DeviceCreate) (*api.Device, error) {
	var d api.Device
	_, err := c.do(ctx, http.MethodPost, api.PathDevices(), nil, req, &d)
	return &d, err
}

// DeleteDevice deletes a device.
func (c *Client) DeleteDevice(ctx context.Context, name string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	_, err := c.do(ctx, http.MethodDelete, api.PathDevice(name), q, nil, nil)
	return err
}

// Attach attaches a device to a host. It returns the HTTP status: 201
// attached now, 200 already attached, 202 attaching in the background.
func (c *Client) Attach(ctx context.Context, name string, req api.AttachRequest) (*api.Attachment, int, error) {
	var a api.Attachment
	status, err := c.do(ctx, http.MethodPost, api.PathDeviceAttachments(name), nil, req, &a)
	if err != nil {
		ErrorObject(err, &a)
	}
	return &a, status, err
}

// DetachOptions are the options of Detach. Force forgets a failed
// attachment.
type DetachOptions struct {
	Timeout time.Duration
	Force   bool
}

// Detach detaches a device from a host. If the guest has not released the
// device within the timeout, the error is a Conflict and the returned
// attachment is in state "failed".
func (c *Client) Detach(ctx context.Context, name, host string, o DetachOptions) (*api.Attachment, int, error) {
	q := url.Values{}
	if o.Timeout > 0 {
		q.Set("timeout", o.Timeout.String())
	}
	if o.Force {
		q.Set("force", "true")
	}
	var a api.Attachment
	status, err := c.do(ctx, http.MethodDelete, api.PathDeviceAttachment(name, host), q, nil, &a)
	if err != nil {
		ErrorObject(err, &a)
	}
	return &a, status, err
}

// Attachments lists attachments, optionally of a host and/or device.
func (c *Client) Attachments(ctx context.Context, host, device string) ([]api.Attachment, error) {
	q := url.Values{}
	if host != "" {
		q.Set("host", host)
	}
	if device != "" {
		q.Set("device", device)
	}
	var as []api.Attachment
	_, err := c.do(ctx, http.MethodGet, api.PathAttachments(), q, nil, &as)
	return as, err
}

// Event is an event with an undecoded object.
type Event struct {
	Type   string          `json:"type"`
	Time   time.Time       `json:"time"`
	Object json.RawMessage `json:"object"`
}

// Events streams server events to fn until ctx is done or fn returns an
// error.
func (c *Client) Events(ctx context.Context, fn func(Event) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+api.PathEvents(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("events: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}
