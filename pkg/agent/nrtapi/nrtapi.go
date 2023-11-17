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

package nrtapi

import (
	"net/http"

	"k8s.io/client-go/rest"

	api "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha2"
	scheme "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/scheme"
	client "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/generated/clientset/versioned/typed/topology/v1alpha2"
)

// Clientset is the client for accessing node resource topology custom resources.
type Client = client.TopologyV1alpha2Client

// NewForConfigAndClient creates a new Clientset for the given config and http client.
func NewForConfigAndClient(c *rest.Config, httpCli *http.Client) (*Client, error) {
	restCfg := *c
	if err := setConfigDefaults(&restCfg); err != nil {
		return nil, err
	}

	cli, err := rest.RESTClientForConfigAndClient(&restCfg, httpCli)
	if err != nil {
		return nil, err
	}

	return client.New(cli), nil
}

func setConfigDefaults(config *rest.Config) error {
	gv := api.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return nil
}
