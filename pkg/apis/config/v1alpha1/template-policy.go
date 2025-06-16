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

package v1alpha1

import "github.com/intel/goresctrl/pkg/rdt"

var (
	_ ResmgrConfig = &TemplatePolicy{}
)

func (c *TemplatePolicy) AgentConfig() *AgentConfig {
	if c == nil {
		return nil
	}

	a := c.Spec.Agent

	return &a
}

func (c *TemplatePolicy) CommonConfig() *CommonConfig {
	if c == nil {
		return nil
	}
	return &CommonConfig{
		Control:         c.Spec.Control,
		Log:             c.Spec.Log,
		Instrumentation: c.Spec.Instrumentation,
	}
}

func (c *TemplatePolicy) PolicyConfig() interface{} {
	if c == nil {
		return nil
	}
	return &c.Spec.Config
}

func (c *TemplatePolicy) RdtConfig() (*rdt.Config, bool, error) {
	return nil, false, nil
}
