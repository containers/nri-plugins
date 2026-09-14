/*
Copyright The NRI Plugins Authors.

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

// Package client builds a *kubernetes.Clientset from a kubeconfig file or
// in-cluster credentials, and exposes the REST config and HTTP client it
// was built from so callers can share one client.
package client

import (
	"errors"
	"net/http"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Wire content types accepted by the Kubernetes API server.
const (
	ContentTypeJSON     = "application/json"
	ContentTypeProtobuf = "application/vnd.kubernetes.protobuf"
)

// Client wraps a Kubernetes clientset together with the REST config and
// HTTP client it was built from. Use the embedded Clientset directly for
// API calls, or HttpClient()/RestConfig() to build other clients sharing
// the same transport.
type Client struct {
	cfg  *rest.Config
	http *http.Client
	*kubernetes.Clientset
}

// Option configures a Client during construction via New. Options apply
// in order; config-dependent options (WithContentType,
// WithAcceptContentTypes) require a config-source option (WithKubeConfig,
// WithInClusterConfig, or WithRestConfig) earlier in the list.
type Option func(*Client) error

// errNoConfigSet is returned by options that require the REST config
// to be present but are called before any config-source option.
var errNoConfigSet = errors.New("option requires REST config; pass a config-source option (WithKubeConfig, WithInClusterConfig, or WithRestConfig) before this option")

// GetConfigForFile returns a REST configuration parsed from the given
// kubeconfig file path. Thin wrapper over clientcmd.BuildConfigFromFlags
// exposed for callers that need a config but not a full Client.
func GetConfigForFile(kubeConfig string) (*rest.Config, error) {
	return clientcmd.BuildConfigFromFlags("", kubeConfig)
}

// InClusterConfig returns the REST configuration for the pod's service
// account, if the process is running inside a Kubernetes cluster.
// Returns rest.ErrNotInCluster (wrapped) when not in a cluster.
func InClusterConfig() (*rest.Config, error) {
	return rest.InClusterConfig()
}

// New constructs a Client by applying the given options in order,
// defaulting to WithInClusterConfig() if none set a REST config.
func New(options ...Option) (*Client, error) {
	c := &Client{}

	for _, o := range options {
		if err := o(c); err != nil {
			return nil, err
		}
	}

	if c.cfg == nil {
		if err := WithInClusterConfig()(c); err != nil {
			return nil, err
		}
	}

	if c.http == nil {
		hc, err := rest.HTTPClientFor(c.cfg)
		if err != nil {
			return nil, err
		}
		c.http = hc
	}

	cs, err := kubernetes.NewForConfigAndClient(c.cfg, c.http)
	if err != nil {
		return nil, err
	}
	c.Clientset = cs

	return c, nil
}

// WithKubeConfig returns an Option that resolves the REST config from
// the given kubeconfig file.
func WithKubeConfig(file string) Option {
	return func(c *Client) error {
		cfg, err := GetConfigForFile(file)
		if err != nil {
			return err
		}
		return WithRestConfig(cfg)(c)
	}
}

// WithInClusterConfig returns an Option that resolves the REST config
// from the pod's service-account credentials.
func WithInClusterConfig() Option {
	return func(c *Client) error {
		cfg, err := InClusterConfig()
		if err != nil {
			return err
		}
		return WithRestConfig(cfg)(c)
	}
}

// WithKubeOrInClusterConfig resolves the REST config from the given
// kubeconfig file if non-empty, or from in-cluster credentials otherwise.
func WithKubeOrInClusterConfig(file string) Option {
	if file == "" {
		return WithInClusterConfig()
	}
	return WithKubeConfig(file)
}

// WithRestConfig uses a deep copy (via rest.CopyConfig) of the given REST
// config, so the caller keeps ownership of the original.
func WithRestConfig(cfg *rest.Config) Option {
	return func(c *Client) error {
		if cfg == nil {
			return errors.New("rest config must not be nil")
		}
		c.cfg = rest.CopyConfig(cfg)
		return nil
	}
}

// WithHttpClient returns an Option that uses the given pre-built HTTP
// client. Useful when multiple components should share one client
// (and therefore its connection pool).
func WithHttpClient(hc *http.Client) Option {
	return func(c *Client) error {
		c.http = hc
		return nil
	}
}

// WithAcceptContentTypes sets the Accept content types to negotiate with
// the API server, joined with commas. Requires a config-source option
// earlier in the list.
func WithAcceptContentTypes(contentTypes ...string) Option {
	return func(c *Client) error {
		if c.cfg == nil {
			return errNoConfigSet
		}
		c.cfg.AcceptContentTypes = strings.Join(contentTypes, ",")
		return nil
	}
}

// WithContentType sets the wire content type used for requests. Requires
// a config-source option earlier in the list.
func WithContentType(contentType string) Option {
	return func(c *Client) error {
		if c.cfg == nil {
			return errNoConfigSet
		}
		c.cfg.ContentType = contentType
		return nil
	}
}

// RestConfig returns a copy of the Client's REST config. Top-level and
// value-typed nested fields may be freely overwritten, but nested
// maps/slices (e.g. TLSClientConfig.CAData) share storage with the
// Client's internal config and must not be mutated.
func (c *Client) RestConfig() *rest.Config {
	return rest.CopyConfig(c.cfg)
}

// HttpClient returns the Client's underlying HTTP client, e.g. for
// constructing other clients that share the same transport.
func (c *Client) HttpClient() *http.Client {
	return c.http
}

// K8sClient returns the Client's underlying *kubernetes.Clientset.
// Callers may alternatively use the embedded Clientset directly on
// the Client value.
func (c *Client) K8sClient() *kubernetes.Clientset {
	return c.Clientset
}

// Close releases resources held by the Client. Safe to call on a nil or
// already-closed Client.
func (c *Client) Close() {
	if c == nil {
		return
	}
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	c.cfg = nil
	c.http = nil
	c.Clientset = nil
}
