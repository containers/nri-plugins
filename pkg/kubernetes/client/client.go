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

// Package client provides a Kubernetes client wrapper used by nri-plugins
// to construct a fully-configured *kubernetes.Clientset from either a
// kubeconfig file or in-cluster credentials. The wrapper exposes the
// underlying REST config, HTTP client, and clientset so downstream
// consumers (agent, DRA driver, ...) can share a single configured
// client rather than each rebuilding their own.
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
// HTTP client it was built from. Callers can use the embedded Clientset
// directly for API calls, or reach for HttpClient()/RestConfig() when
// they need to construct additional clients (e.g. an NRT client) that
// should share the same underlying transport.
type Client struct {
	cfg  *rest.Config
	http *http.Client
	*kubernetes.Clientset
}

// Option is a functional option that can be applied to a Client during
// construction via New. Options are applied in the order they are
// passed. Config-dependent options (such as WithContentType and
// WithAcceptContentTypes) require a REST config to already be present,
// so a config-source option (WithKubeConfig, WithInClusterConfig, or
// WithRestConfig) must appear before them in the option list.
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

// New constructs a Client by applying the given options in order. If no
// option sets a REST config, New falls back to WithInClusterConfig()
// automatically. Config-dependent options (e.g. WithContentType,
// WithAcceptContentTypes) require the REST config to already be set,
// so a config-source option must appear before them; violating this
// ordering returns an error immediately.
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

// WithKubeOrInClusterConfig returns an Option that resolves the REST
// config from the given kubeconfig file if the path is non-empty, or
// falls back to in-cluster credentials otherwise. This is the typical
// choice for daemons that can run inside or outside a cluster.
func WithKubeOrInClusterConfig(file string) Option {
	if file == "" {
		return WithInClusterConfig()
	}
	return WithKubeConfig(file)
}

// WithRestConfig returns an Option that uses the given pre-built REST
// config. The input is deep-copied via rest.CopyConfig so the caller
// retains full ownership of the original and post-New mutations do
// not leak into the client.
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

// WithAcceptContentTypes returns an Option that sets the Accept content
// types the client will negotiate with the API server. Multiple values
// are joined with commas. Requires the REST config to be present; a
// config-source option (WithKubeConfig, WithInClusterConfig, or
// WithRestConfig) must appear before this option.
func WithAcceptContentTypes(contentTypes ...string) Option {
	return func(c *Client) error {
		if c.cfg == nil {
			return errNoConfigSet
		}
		c.cfg.AcceptContentTypes = strings.Join(contentTypes, ",")
		return nil
	}
}

// WithContentType returns an Option that sets the wire content type the
// client uses for requests. Requires the REST config to be present; a
// config-source option (WithKubeConfig, WithInClusterConfig, or
// WithRestConfig) must appear before this option.
func WithContentType(contentType string) Option {
	return func(c *Client) error {
		if c.cfg == nil {
			return errNoConfigSet
		}
		c.cfg.ContentType = contentType
		return nil
	}
}

// RestConfig returns a copy of the Client's REST configuration produced
// by rest.CopyConfig. Callers may overwrite top-level fields (Host,
// APIPath, UserAgent, ...) and value-typed nested structs
// (TLSClientConfig, Impersonate, ContentConfig, ...) freely. Callers
// must NOT mutate the contents of nested maps or slices
// (Impersonate.Extra, TLSClientConfig.CAData, etc.) — those share
// storage with the Client's internal config. This matches the
// Kubernetes-ecosystem convention.
func (c *Client) RestConfig() *rest.Config {
	return rest.CopyConfig(c.cfg)
}

// HttpClient returns the Client's underlying HTTP client. Callers can
// use it to construct additional clients (e.g. an NRT client via
// nrtapi.NewForConfigAndClient) that share the same transport.
func (c *Client) HttpClient() *http.Client {
	return c.http
}

// K8sClient returns the Client's underlying *kubernetes.Clientset.
// Callers may alternatively use the embedded Clientset directly on
// the Client value.
func (c *Client) K8sClient() *kubernetes.Clientset {
	return c.Clientset
}

// Close releases resources held by the Client. It is idempotent:
// calling Close on a nil Client or a previously-closed Client is a
// no-op that does not panic.
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
