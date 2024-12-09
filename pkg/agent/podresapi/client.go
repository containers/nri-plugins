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

package podresapi

import (
	"context"
	"fmt"
	"strings"

	logger "github.com/containers/nri-plugins/pkg/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	api "k8s.io/kubelet/pkg/apis/podresources/v1"
)

// ClientOption is an option for the client.
type ClientOption func(*Client)

// Client is Pod Resources API client.
type Client struct {
	socketPath string
	conn       *grpc.ClientConn
	cli        api.PodResourcesListerClient
	noGet      bool
}

const (
	// these constants were obtained from NFD sources, cross-checked against
	//   https://github.com/kubernetes/kubernetes/blob/release-1.31/test/e2e_node/util.go#L83
	defaultSocketPath = "/var/lib/kubelet/pod-resources/kubelet.sock"
	maxSize           = 1024 * 1024 * 16
)

var (
	errGetDisabled = fmt.Errorf("PodResources API Get method disabled")
	log            = logger.Get("podresapi")
)

// WithSocketPath sets the kubelet socket path to connect to.
func WithSocketPath(path string) ClientOption {
	return func(c *Client) {
		c.socketPath = path
	}
}

// WithClientConn sets a pre-created gRPC connection for the client.
func WithClientConn(conn *grpc.ClientConn) ClientOption {
	return func(c *Client) {
		c.conn = conn
	}
}

// NewClient creates a new Pod Resources API client with the given options.
func NewClient(options ...ClientOption) (*Client, error) {
	c := &Client{
		socketPath: defaultSocketPath,
	}

	for _, o := range options {
		o(c)
	}

	if c.conn == nil {
		conn, err := grpc.NewClient("unix://"+c.socketPath,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxSize)),
		)

		if err != nil {
			return nil, fmt.Errorf("failed to connect podresource client: %w", err)
		}

		c.conn = conn
	}

	c.cli = api.NewPodResourcesListerClient(c.conn)
	return c, nil
}

// Close closes the client.
func (c *Client) Close() {
	if c != nil && c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.cli = nil
}

// HasClient returns true if the client has a usable client.
func (c *Client) HasClient() bool {
	return c != nil && c.cli != nil
}

// List lists all pods' resources.
func (c *Client) List(ctx context.Context) (PodResourcesList, error) {
	if !c.HasClient() {
		return nil, nil
	}

	reply, err := c.cli.List(ctx, &api.ListPodResourcesRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod resources by list: %w", err)
	}

	return PodResourcesList(reply.GetPodResources()), nil
}

// Get queries the given pod's resources.
func (c *Client) Get(ctx context.Context, namespace, pod string) (*PodResources, error) {
	if !c.HasClient() {
		return nil, nil
	}

	if !c.noGet {
		reply, err := c.cli.Get(ctx, &api.GetPodResourcesRequest{
			PodNamespace: namespace,
			PodName:      pod,
		})
		if err == nil {
			return &PodResources{reply.GetPodResources()}, nil
		}

		if !strings.Contains(fmt.Sprintf("%v", err), fmt.Sprintf("%v", errGetDisabled)) {
			return nil, fmt.Errorf("failed to get pod resources: %w", err)
		}

		log.Warnf("PodResources API Get() disabled, falling back to List()...")
		log.Warnf("You can enable Get() by passing this feature gate setting to kubelet:")
		log.Warnf("  --feature-gates=KubeletPodResourcesGet=true")

		c.noGet = true
	}

	l, err := c.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod resources: %w", err)
	}

	return l.GetPodResources(namespace, pod), nil
}
