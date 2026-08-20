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
	"net/http"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
// construction via New. Options are applied in order; those that depend
// on an already-resolved REST config return errRetryWhenConfigSet on
// their first invocation and are re-applied after the config source
// (WithKubeConfig / WithInClusterConfig / WithRestConfig) has run, so
// callers can pass options in any order.
type Option func(*Client) error

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
