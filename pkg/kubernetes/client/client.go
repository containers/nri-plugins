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

package client

import (
	"errors"
	"net/http"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Option is an option that can be applied to a Client.
type Option func(*Client) error

// Client enacapsulates our Kubernetes client.
type Client struct {
	cfg  *rest.Config
	http *http.Client
	*kubernetes.Clientset
}

// GetConfigForFile returns a REST configuration for the given file.
func GetConfigForFile(kubeConfig string) (*rest.Config, error) {
	return clientcmd.BuildConfigFromFlags("", kubeConfig)
}

// InClusterConfig returns the in-cluster REST configuration.
func InClusterConfig() (*rest.Config, error) {
	return rest.InClusterConfig()
}

// WithKubeConfig returns a Client Option for using the given kubeconfig file.
func WithKubeConfig(file string) Option {
	return func(c *Client) error {
		cfg, err := GetConfigForFile(file)
		if err != nil {
			return err
		}
		return WithRestConfig(cfg)(c)
	}
}

// WithInClusterConfig returns a Client Option for using the in-cluster configuration.
func WithInClusterConfig() Option {
	return func(c *Client) error {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return err
		}
		return WithRestConfig(cfg)(c)
	}
}

// WithKubeOrInClusterConfig returns a Client Option for using in-cluster configuration
// if a configuration file is not given.
func WithKubeOrInClusterConfig(file string) Option {
	if file == "" {
		return WithInClusterConfig()
	}
	return WithKubeConfig(file)
}

// WithRestConfig returns a Client Option for using the given REST configuration.
func WithRestConfig(cfg *rest.Config) Option {
	return func(c *Client) error {
		c.cfg = rest.CopyConfig(cfg)
		return nil
	}
}

// WithHttpClient returns a Client Option for using/sharing the given HTTP client.
func WithHttpClient(hc *http.Client) Option {
	return func(c *Client) error {
		c.http = hc
		return nil
	}
}

// WithAcceptContentTypes returns a Client Option for setting the accepted content types.
func WithAcceptContentTypes(contentTypes ...string) Option {
	return func(c *Client) error {
		if c.cfg == nil {
			return errRetryWhenConfigSet
		}
		c.cfg.AcceptContentTypes = strings.Join(contentTypes, ",")
		return nil
	}
}

// WithContentType returns a Client Option for setting the wire format content type.
func WithContentType(contentType string) Option {
	return func(c *Client) error {
		if c.cfg == nil {
			return errRetryWhenConfigSet
		}
		c.cfg.ContentType = contentType
		return nil
	}
}

const (
	ContentTypeJSON     = "application/json"
	ContentTypeProtobuf = "application/vnd.kubernetes.protobuf"
)

var (
	// returned by options if applied too early, before a configuration is set
	errRetryWhenConfigSet = errors.New("retry when client config is set")
)

// New creates a new Client with the given options.
func New(options ...Option) (*Client, error) {
	c := &Client{}

	var retry []Option
	for _, o := range options {
		if err := o(c); err != nil {
			if err == errRetryWhenConfigSet {
				retry = append(retry, o)
			} else {
				return nil, err
			}
		}
	}

	if c.cfg == nil {
		if err := WithInClusterConfig()(c); err != nil {
			return nil, err
		}
	}

	for _, o := range retry {
		if err := o(c); err != nil {
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

	client, err := kubernetes.NewForConfigAndClient(c.cfg, c.http)
	if err != nil {
		return nil, err
	}
	c.Clientset = client

	return c, nil
}

// RestConfig returns a shallow copy of the REST configuration of the Client.
func (c *Client) RestConfig() *rest.Config {
	cfg := *c.cfg
	return &cfg
}

// HttpClient returns the HTTP client of the Client.
func (c *Client) HttpClient() *http.Client {
	return c.http
}

// K8sClient returns the K8s Clientset of the Client.
func (c *Client) K8sClient() *kubernetes.Clientset {
	return c.Clientset
}

// Close closes the Client.
func (c *Client) Close() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
	c.cfg = nil
	c.http = nil
	c.Clientset = nil
}
