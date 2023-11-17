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

package agent

import (
	"context"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	cfgapi "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1"
	client "github.com/containers/nri-plugins/pkg/generated/clientset/versioned"
)

type configKind int

const (
	balloonsConfig configKind = iota
	topologyAwareConfig
	templateConfig
)

type configIf struct {
	kind configKind
	cfg  *rest.Config
	cli  *client.Clientset
}

func newConfigIf(kind configKind) *configIf {
	return &configIf{
		kind: kind,
	}
}

func (cif *configIf) SetKubeClient(httpCli *http.Client, restCfg *rest.Config) error {
	cfg := *restCfg
	cli, err := client.NewForConfigAndClient(&cfg, httpCli)
	if err != nil {
		return fmt.Errorf("failed to create client for config resource access: %w", err)
	}

	cif.cfg = &cfg
	cif.cli = cli
	return nil
}

func (cif *configIf) CreateWatch(ctx context.Context, ns, name string) (watch.Interface, error) {
	selector := metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	}

	switch cif.kind {
	case balloonsConfig:
		return cif.cli.ConfigV1alpha1().BalloonsPolicies(ns).Watch(ctx, selector)
	case topologyAwareConfig:
		return cif.cli.ConfigV1alpha1().TopologyAwarePolicies(ns).Watch(ctx, selector)
	case templateConfig:
		return cif.cli.ConfigV1alpha1().TemplatePolicies(ns).Watch(ctx, selector)
	}
	return nil, fmt.Errorf("configIf: unknown config type %v", cif.kind)
}

func (cif *configIf) PatchStatus(ctx context.Context, ns, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) error {
	if cif.cli == nil {
		return nil
	}

	var err error

	switch cif.kind {
	case balloonsConfig:
		_, err = cif.cli.ConfigV1alpha1().BalloonsPolicies(ns).Patch(ctx, name, pt, data, opts, "status")
	case topologyAwareConfig:
		_, err = cif.cli.ConfigV1alpha1().TopologyAwarePolicies(ns).Patch(ctx, name, pt, data, opts, "status")
	case templateConfig:
		_, err = cif.cli.ConfigV1alpha1().TemplatePolicies(ns).Patch(ctx, name, pt, data, opts, "status")
	}

	if err != nil {
		return fmt.Errorf("patching status failed: %v", err)
	}

	return nil
}

func (cif *configIf) Unmarshal(data []byte, file string) (runtime.Object, error) {
	var (
		obj runtime.Object
		err error
	)

	switch cif.kind {
	case balloonsConfig:
		cfg := &cfgapi.BalloonsPolicy{}
		if err = yaml.UnmarshalStrict(data, cfg); err == nil {
			cfg.Name = file + ":" + cfg.Name
			obj = cfg
		}
	case topologyAwareConfig:
		cfg := &cfgapi.TopologyAwarePolicy{}
		if err = yaml.UnmarshalStrict(data, cfg); err == nil {
			cfg.Name = file + ":" + cfg.Name
			obj = cfg
		}
	case templateConfig:
		cfg := &cfgapi.TemplatePolicy{}
		if err = yaml.UnmarshalStrict(data, cfg); err == nil {
			cfg.Name = file + ":" + cfg.Name
			obj = cfg
		}
	}

	if err != nil {
		return nil, err
	}

	return obj, nil
}
