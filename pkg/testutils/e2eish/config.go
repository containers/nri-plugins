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

package e2eish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"sigs.k8s.io/yaml"
)

// srcDir is the root of the source tree.
func srcDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// HelmConfig returns the configuration and the extra environment a helm
// launch of the plugin with the given values files gives the policy. The
// values of the chart are merged with the given ones in order, as helm does,
// and the defaults of the CRD are applied to the configuration, as the API
// server does.
func (p *Plugin) HelmConfig(values ...json.RawMessage) (any, map[string]string, error) {
	chart, err := readYAML(filepath.Join(srcDir(), "deployment", "helm", p.Name, "values.yaml"))
	if err != nil {
		return nil, nil, err
	}
	var merged any = chart
	for _, v := range values {
		var given map[string]any
		if len(v) > 0 {
			if err := json.Unmarshal(v, &given); err != nil {
				return nil, nil, fmt.Errorf("bad helm values: %w", err)
			}
		}
		merged = mergeValues(merged, given)
	}
	valuesMap, _ := merged.(map[string]any)

	crd, err := readYAML(filepath.Join(srcDir(), "deployment", "helm", p.Name, "crds", p.CRD))
	if err != nil {
		return nil, nil, err
	}
	schema := lookup(crd, "spec", "versions", 0, "schema", "openAPIV3Schema", "properties", "spec")
	spec := applyDefaults(valuesMap["config"], schema)

	data, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, err
	}
	cfg := p.Config()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, nil, fmt.Errorf("bad %s configuration: %w", p.Name, err)
	}

	// The chart renders an extraEnv value unquoted into the YAML of the
	// daemonset, which takes off one more level of quotes.
	env := map[string]string{}
	extra, _ := valuesMap["extraEnv"].(map[string]any)
	for name, value := range extra {
		s := fmt.Sprint(value)
		var rendered struct{ Value string }
		if err := yaml.Unmarshal([]byte("value: "+s), &rendered); err == nil {
			s = rendered.Value
		}
		env[name] = s
	}

	return cfg, env, nil
}

func readYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

// mergeValues merges helm values over others, as helm does: maps merge, any
// other value replaces, and null deletes.
func mergeValues(base, over any) any {
	b, bok := base.(map[string]any)
	o, ook := over.(map[string]any)
	if !bok || !ook {
		if over == nil && !ook {
			return base
		}
		return over
	}
	out := map[string]any{}
	for k, v := range b {
		out[k] = v
	}
	for k, v := range o {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergeValues(out[k], v)
	}
	return out
}

// lookup follows keys and indices into decoded YAML or JSON.
func lookup(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			l, _ := v.([]any)
			if k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

// applyDefaults applies the defaults of an OpenAPI schema to a value, as the
// API server does: an absent property with a default gets it, and the
// defaults of properties, map values and list items apply within.
func applyDefaults(v any, schema any) any {
	s, _ := schema.(map[string]any)
	if s == nil {
		return v
	}
	switch val := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for name, ps := range props {
			if _, ok := val[name]; !ok {
				if d, ok := ps.(map[string]any)["default"]; ok {
					val[name] = deepCopy(d)
				}
			}
			if pv, ok := val[name]; ok {
				val[name] = applyDefaults(pv, ps)
			}
		}
		if ap, ok := s["additionalProperties"].(map[string]any); ok {
			for name, pv := range val {
				if _, known := props[name]; !known {
					val[name] = applyDefaults(pv, ap)
				}
			}
		}
		return val
	case []any:
		for i := range val {
			val[i] = applyDefaults(val[i], s["items"])
		}
		return val
	case nil:
		return nil
	default:
		return v
	}
}

func deepCopy(v any) any {
	data, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(data, &out)
	return out
}
