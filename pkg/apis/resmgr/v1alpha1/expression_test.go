// Copyright 2019-2020 Intel Corporation. All Rights Reserved.
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

package resmgr

import (
	"fmt"
	"strings"
	"testing"

	logger "github.com/containers/nri-plugins/pkg/log"
	"github.com/stretchr/testify/require"
)

type evaluable struct {
	name      string
	namespace string
	qosclass  string
	labels    map[string]string
	tags      map[string]string
	parent    Evaluable
}

func newEvaluable(name, ns, qos string, labels, tags map[string]string, p Evaluable) *evaluable {
	return &evaluable{
		name:      name,
		namespace: ns,
		qosclass:  qos,
		labels:    labels,
		tags:      tags,
		parent:    p,
	}
}

func (e *evaluable) EvalKey(key string) interface{} {
	switch key {
	case KeyName:
		return e.name
	case KeyNamespace:
		return e.namespace
	case KeyQOSClass:
		return e.qosclass
	case KeyLabels:
		return e.labels
	case KeyTags:
		return e.tags
	case KeyPod:
		if e.parent != nil {
			return e.parent
		}
		fallthrough
	default:
		return fmt.Errorf("evaluable: cannot evaluate %q", key)
	}
}

func (e *evaluable) EvalRef(key string) (string, bool) {
	return KeyValue(key, e)
}

func (e *evaluable) String() string {
	s := fmt.Sprintf("{ name: %q, namespace: %q, qosclass: %q, ", e.name, e.namespace, e.qosclass)
	labels, t := "{", ""
	for k, v := range e.labels {
		labels += t + fmt.Sprintf("%q:%q", k, v)
		t = ", "
	}
	labels += "}"
	tags, t := "{", ""
	for k, v := range e.tags {
		tags += t + fmt.Sprintf("%q:%q", k, v)
		t = ", "
	}
	tags += "}"
	s = fmt.Sprintf("%s, labels: %s, tags: %s }", s, labels, tags)
	return s
}

func TestResolveRefAndKeyValue(t *testing.T) {
	defer logger.Flush()

	pod := newEvaluable("P1", "pns", "pqos",
		map[string]string{
			"l1":             "plone",
			"l2":             "pltwo",
			"l5":             "plfive",
			"io.test/label1": "io.test/value1",
		}, nil, nil)

	tcases := []struct {
		name      string
		subject   Evaluable
		keys      []string
		values    []string
		ok        []bool
		error     []bool
		keyvalues []string
	}{
		{
			name: "test resolving references",
			subject: newEvaluable("C1", "cns", "cqos",
				map[string]string{
					"l1": "clone",
					"l2": "cltwo",
					"l3": "clthree",
				},
				map[string]string{"t1": "ctone", "t2": "cttwo", "t3": "ctthree"}, pod),
			keys: []string{
				"name", "namespace", "qosclass",
				"labels/l1", "labels/l2", "labels/l3", "labels/l4",
				"tags/t1", "tags/t2", "tags/t3", "tags/t4",
				"pod/labels/l1",
				"pod/labels/l2",
				"pod/labels/l3",
				"pod/labels/l4",
				"pod/labels/l5",
				"pod/labels/io.test/label1",
				":,-pod/qosclass,pod/namespace,pod/name,name",
			},
			values: []string{
				"C1", "cns", "cqos",
				"clone", "cltwo", "clthree", "",
				"ctone", "cttwo", "ctthree", "",
				"plone", "pltwo", "", "", "plfive", "io.test/value1",
				"",
			},
			keyvalues: []string{
				"C1", "cns", "cqos",
				"clone", "cltwo", "clthree", "",
				"ctone", "cttwo", "ctthree", "",
				"plone", "pltwo", "", "", "plfive", "io.test/value1",
				"pqos-pns-P1-C1",
			},
			ok: []bool{
				true, true, true,
				true, true, true, false,
				true, true, true, false,
				true, true, false, false, true, true,
				false,
			},
			error: []bool{
				false, false, false,
				false, false, false, false,
				false, false, false, false,
				false, false, false, false, false, false,
				true,
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.keys {
				value, ok, err := ResolveRef(tc.subject, tc.keys[i])
				if err != nil && !tc.error[i] {
					t.Errorf("ResolveRef %s/%q should have given %q, but failed: %v",
						tc.subject, tc.keys[i], tc.values[i], err)
					continue
				}
				if value != tc.values[i] || ok != tc.ok[i] {
					t.Errorf("ResolveRef %s@%q: expected %v, %v got %v, %v",
						tc.subject, tc.keys[i], tc.values[i], tc.ok[i], value, ok)
					continue
				}
				expr := &Expression{
					Key:    tc.keys[i],
					Op:     Equals,
					Values: []string{},
				}
				value, _ = KeyValue(expr.Key, tc.subject)
				if value != tc.keyvalues[i] {
					t.Errorf("KeyValue %s@%q: expected %v, got %v",
						tc.subject, tc.keys[i], tc.keyvalues[i], value)
				}
			}
		})
	}
}

func TestEvalRef(t *testing.T) {
	defer logger.Flush()

	podLabels := map[string]string{"l1": "pl1", "l2": "pl2", "l5": "pl5", "io.t/l1": "pio.t/l1"}
	pod := newEvaluable("pod1", "pod1-ns", "pod1-qos", podLabels, nil, nil)
	ctrLabels := map[string]string{"l1": "cl1", "l2": "cl2", "l3": "cl3"}
	ctr := newEvaluable("ctr1", "ctr1-ns", "ctr1-qos", ctrLabels, nil, pod)

	tcases := []struct {
		name   string
		keys   []string
		values []string
		ok     []bool
	}{
		{
			name: "test resolving references",
			keys: []string{
				"name", "namespace", "qosclass",
				"labels/l1", "labels/l2", "labels/l3", "labels/l4",
				"pod/labels/l1", "pod/labels/l2", "pod/labels/l3", "pod/labels/l4", "pod/labels/l5",
				"pod/labels/io.t/l", "pod/labels/io.t/l1",
				":,-pod/qosclass,pod/namespace,pod/name,name",
			},
			values: []string{
				"ctr1", "ctr1-ns", "ctr1-qos",
				"cl1", "cl2", "cl3", "",
				"pl1", "pl2", "", "", "pl5",
				"", "pio.t/l1",
				"pod1-qos-pod1-ns-pod1-ctr1",
			},
			ok: []bool{
				true, true, true,
				true, true, true, false,
				true, true, false, false, true,
				false, true,
				true,
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.keys {
				value, ok := ctr.EvalRef(tc.keys[i])
				if !ok && tc.ok[i] {
					t.Errorf("EvalRef %q for %s should have given %q, but failed",
						tc.keys[i], ctr, tc.values[i])
					continue
				}
				if value != tc.values[i] || ok != tc.ok[i] {
					t.Errorf("EvalRef %q for %s: expected %v, %v got %v, %v",
						tc.keys[i], ctr, tc.values[i], tc.ok[i], value, ok)
				}
			}
		})
	}
}

func TestSimpleOperators(t *testing.T) {
	defer logger.Flush()

	pod := newEvaluable("P1", "pns", "pqos",
		map[string]string{"l1": "plone", "l2": "pltwo", "l5": "plfive"},
		nil,
		nil)
	sub := newEvaluable("C1", "cns", "cqos",
		map[string]string{"l1": "clone", "l2": "cltwo", "l3": "clthree"},
		map[string]string{"t1": "ctone", "t2": "cttwo", "t4": "ctfour"},
		pod)

	tcases := []struct {
		name    string
		subject Evaluable
		keys    []string
		ops     []Operator
		values  [][][]string
		results [][]bool
	}{
		{
			name:    "test Equals, NotEqual, In, NotIn operators",
			subject: sub,
			keys: []string{
				"name",
				"pod/name",
				"namespace",
				"pod/namespace",
				"qosclass",
				"pod/qosclass",
				"labels/l1",
				"labels/l2",
				"labels/l3",
				"labels/l4",
				"tags/t1",
				"tags/t2",
				"tags/t3",
				"tags/t4",
				"pod/labels/l1",
				"pod/labels/l2",
				"pod/labels/l3",
				"pod/labels/l4",
				"pod/labels/l5",
			},
			ops: []Operator{Equals, NotEqual, In, NotIn},
			values: [][][]string{
				{{"C1"}, {"C1"}, {"foo", "C1"}, {"foo"}},                    // name
				{{"P1"}, {"P1"}, {"foo", "P1"}, {"foo"}},                    // pod/name
				{{"cns"}, {"cns"}, {"foo", "cns"}, {"foo"}},                 // namespace
				{{"pns"}, {"pns"}, {"foo", "pns"}, {"pns"}},                 // pod/namespace
				{{"cqos"}, {"cqos"}, {"foo", "cqos"}, {"foo"}},              // qosclass
				{{"pqos"}, {"pqos"}, {"foo", "pqos"}, {"pqos"}},             // pod/qosclass
				{{"clone"}, {"clone"}, {"foo", "clone"}, {"foo"}},           // labels/l1
				{{"cltwo"}, {"cltwo"}, {"foo", "cltwo"}, {"foo"}},           // labels/l2
				{{"clthree"}, {"clthree"}, {"foo", "clthree"}, {"clthree"}}, // labels/l3
				{{"clfour"}, {"clfour"}, {"foo", "clfour"}, {"foo"}},        // labels/l4
				{{"ctone"}, {"ctone"}, {"foo", "ctone"}, {"foo"}},           // tags/t1
				{{"cttwo"}, {"cttwo"}, {"foo", "cttwo"}, {"foo"}},           // tags/t2
				{{"ctthree"}, {"ctthree"}, {"foo", "ctthree"}, {"foo"}},     // tags/t3
				{{"ctfour"}, {"ctfour"}, {"foo", "ctfour"}, {"ctfour"}},     // tags/t4
				{{"plone"}, {"plone"}, {"foo", "plone"}, {"foo"}},           // pod/labels/l1
				{{"pltwo"}, {"pltwo"}, {"foo", "pltwo"}, {"foo"}},           // pod/labels/l2
				{{"plthree"}, {"plthree"}, {"foo", "plthree"}, {"foo"}},     // pod/labels/l3
				{{"plfour"}, {"plfour"}, {"foo", "plfour"}, {"foo"}},        // pod/labels/l4
				{{"plfive"}, {"plfive"}, {"foo", "plfive"}, {"foo"}},        // pod/labels/l5
			},
			results: [][]bool{
				{true, false, true, true},  // name
				{true, false, true, true},  // pod/name
				{true, false, true, true},  // namespace
				{true, false, true, false}, // pod/namespace
				{true, false, true, true},  // qosclass
				{true, false, true, false}, // pod/qosclass
				{true, false, true, true},  // labels/l1
				{true, false, true, true},  // labels/l2
				{true, false, true, false}, // labels/l3
				{false, true, false, true}, // labels/l4
				{true, false, true, true},  // tags/t1
				{true, false, true, true},  // tags/t2
				{false, true, false, true}, // tags/t3
				{true, false, true, false}, // tags/t4
				{true, false, true, true},  // pod/labels/l1
				{true, false, true, true},  // pod/labels/l2
				{false, true, false, true}, // pod/labels/l3
				{false, true, false, true}, // pod/labels/l4
				{true, false, true, true},  // pod/labels/l5
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			for k := range tc.keys {
				for o := range tc.ops {
					expr := &Expression{
						Key:    tc.keys[k],
						Op:     tc.ops[o],
						Values: tc.values[k][o],
					}
					expect := tc.results[k][o]
					result := expr.Evaluate(tc.subject)
					if result != expect {
						t.Errorf("%s for %s: expected %v, got %v", expr, tc.subject, expect, result)
					}
				}
			}
		})
	}
}

func TestMatching(t *testing.T) {
	defer logger.Flush()

	p1 := newEvaluable("P1", "pns1", "pqos1",
		map[string]string{"l1": "plv1", "l2": "plv2", "l5": "plv5"},
		nil,
		nil)
	c11 := newEvaluable("C11", "cns1", "cqos11",
		map[string]string{"l1": "clv1", "l2": "clv2", "l3": "clv3"},
		map[string]string{"t1": "ctv1", "t2": "tag2", "t4": "ctv4"},
		p1)
	c12 := newEvaluable("C12", "cns1", "cqos12",
		map[string]string{"l1": "clv1", "l2": "clv2", "l3": "clv3"},
		map[string]string{"t1": "ctv1", "t2": "foo", "t4": "ctv4"},
		p1)
	c13 := newEvaluable("C12", "cns1", "cqos13",
		map[string]string{"l1": "clv1", "l2": "clv2", "l3": "clv3"},
		map[string]string{"t1": "ctv1", "t2": "ctv2", "t4": "ctv4"},
		p1)

	p2 := newEvaluable("P2", "pns2", "pqos2",
		map[string]string{"l1": "plv1", "l2": "plv2", "l5": "plv5"},
		nil,
		nil)
	c21 := newEvaluable("C21", "cns1", "cqos21",
		map[string]string{"l1": "clv1", "l2": "clv2", "l3": "clv3"},
		map[string]string{"t1": "ctv1", "t2": "tag2", "t4": "ctv4"},
		p2)
	c22 := newEvaluable("C22", "cns1", "cqos22",
		map[string]string{"l1": "clv1", "l2": "clv2", "l3": "clv3"},
		map[string]string{"t1": "ctv1", "t2": "ctv2", "t4": "ctv4"},
		p2)
	c23 := newEvaluable("C23", "cns1", "cqos23",
		map[string]string{"l1": "clv1", "l2": "clv2", "l3": "clv3"},
		map[string]string{"t1": "ctv1", "t2": "foo", "t4": "ctv4"},
		p2)

	p3 := newEvaluable("P3", "pns3", "pqos3",
		map[string]string{"l1": "plv1", "l2": "plv2", "l5": "plv5"},
		nil,
		nil)
	c3 := newEvaluable("C3", "cns3", "cqos3",
		map[string]string{"l1": "clv1", "l2": "clv2", "l3": "clv3"},
		map[string]string{"t1": "ctv1", "t2": "tag2", "t4": "ctv4"},
		p3)

	tcases := []struct {
		name      string
		subjects  []Evaluable
		selectors []*Expression
		expected  [][]string
	}{
		{
			name:     "test inverted membership operator",
			subjects: []Evaluable{c11, c12, c13, c21, c22, c23, c3},
			selectors: []*Expression{
				{
					Key: ":,:pod/qosclass,pod/namespace,pod/name,qosclass,name",
					Op:  Matches,
					Values: []string{
						"pqos2:*:*:*:*",
					},
				},
				{
					Key:    "tags/t2",
					Op:     Matches,
					Values: []string{"[tf][ao][go]*"},
				},
			},
			expected: [][]string{
				{"C21", "C22", "C23"},
				{"C11", "C12", "C21", "C23", "C3"},
			},
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			for i, expr := range tc.selectors {
				results := []string{}
				for _, s := range tc.subjects {
					if expr.Evaluate(s) {
						results = append(results, s.EvalKey("name").(string))
					}
				}
				expected := strings.Join(tc.expected[i], ",")
				got := strings.Join(results, ",")
				if expected != got {
					t.Errorf("%s: expected %s, got %s", expr, expected, got)
				}
			}
		})
	}
}

func TestValidation(t *testing.T) {
	defer logger.Flush()

	for _, tc := range []*struct {
		name    string
		expr    *Expression
		invalid bool
	}{
		{
			name: "valid ID reference",
			expr: &Expression{
				Key:    "id",
				Op:     Equals,
				Values: []string{"a"},
			},
		},
		{
			name: "valid uid reference",
			expr: &Expression{
				Key:    "uid",
				Op:     Equals,
				Values: []string{"a"},
			},
		},
		{
			name: "valid name reference",
			expr: &Expression{
				Key:    "name",
				Op:     Equals,
				Values: []string{"a"},
			},
		},
		{
			name: "valid namespace reference",
			expr: &Expression{
				Key:    "namespace",
				Op:     Equals,
				Values: []string{"a"},
			},
		},
		{
			name: "valid QoS class reference",
			expr: &Expression{
				Key:    "qosclass",
				Op:     Equals,
				Values: []string{"a"},
			},
		},
		{
			name: "valid label reference",
			expr: &Expression{
				Key:    "labels/io.kubernetes.application",
				Op:     Equals,
				Values: []string{"test"},
			},
		},
		{
			name: "valid pod reference",
			expr: &Expression{
				Key:    "pod/name",
				Op:     Equals,
				Values: []string{"test"},
			},
		},
		{
			name: "invalid pod reference, no trailing key",
			expr: &Expression{
				Key:    "pod",
				Op:     Equals,
				Values: []string{"test"},
			},
			invalid: true,
		},
		{
			name: "invalid pod reference, unknown trailing key",
			expr: &Expression{
				Key:    "pod/foo",
				Op:     Equals,
				Values: []string{"test"},
			},
			invalid: true,
		},
		{
			name: "invalid name reference, trailing key",
			expr: &Expression{
				Key:    "name/foo",
				Op:     Equals,
				Values: []string{"a"},
			},
			invalid: true,
		},
		{
			name: "invalid equal, wrong number of arguments",
			expr: &Expression{
				Key:    "name",
				Op:     Equals,
				Values: []string{},
			},
			invalid: true,
		},
		{
			name: "invalid NotEqual, wrong number of arguments",
			expr: &Expression{
				Key:    "name",
				Op:     NotEqual,
				Values: []string{"a", "b"},
			},
			invalid: true,
		},
		{
			name: "invalid Matches, wrong number of arguments",
			expr: &Expression{
				Key:    "name",
				Op:     Matches,
				Values: []string{},
			},
			invalid: true,
		},
		{
			name: "invalid MatchesNot, wrong number of arguments",
			expr: &Expression{
				Key:    "name",
				Op:     MatchesNot,
				Values: []string{"a", "b"},
			},
			invalid: true,
		},
		{
			name: "invalid AlwaysTrue, wrong number of arguments",
			expr: &Expression{
				Key:    "name",
				Op:     AlwaysTrue,
				Values: []string{"c"},
			},
			invalid: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.expr.Validate()
			if tc.invalid {
				require.NotNil(t, err)
			} else {
				require.Nil(t, err)
			}
		})
	}
}

func TestExpand(t *testing.T) {
	defer logger.Flush()

	podLabels := map[string]string{"l1": "pl1v", "l2": "pl2v", "l5": "pl5v", "io.t/l1": "pio.t-l1v"}
	pod := newEvaluable("pod1", "pod1-ns", "pod1-qos", podLabels, nil, nil)
	ctrLabels := map[string]string{"l1": "cl1", "l2": "cl2", "l3": "cl3"}
	ctr := newEvaluable("ctr1", "ctr1-ns", "ctr1-qos", ctrLabels, nil, pod)

	tcases := []struct {
		name        string
		source      string
		mustResolve bool
		result      string
		fail        bool
	}{
		{
			name:   "single well-formed key",
			source: "${pod/qosclass}",
			result: "pod1-qos",
		},
		{
			name:   "multiple well-formed keys concatenated",
			source: "${pod/namespace}${pod/name}${pod/qosclass}",
			result: "pod1-nspod1pod1-qos",
		},
		{
			name:   "multiple well-formed keys, with non-empty separators",
			source: "${pod/labels/io.t/l1}:${pod/labels/l2}",
			result: "pio.t-l1v:pl2v",
		},
		{
			name:   "single plain key",
			source: "$pod/qosclass",
			result: "pod1-qos",
		},
		{
			name:   "multiple plain keys",
			source: "$pod/namespace$pod/name$pod/qosclass",
			result: "pod1-nspod1pod1-qos",
		},
		{
			name:   "multiple plain keys concatenated",
			source: "$pod/labels/io.t/l1$pod/labels/l2",
			result: "pio.t-l1vpl2v",
		},
		{
			name:   "unresolvable keys",
			source: "${pod/foobar}${xyzzy}",
			result: "",
		},
		{
			name:   "mixed resolvable and unresolvable keys",
			source: "${pod/labels/l5}$foobar${pod/name}",
			result: "pl5vpod1",
		},
		{
			name:        "unresolvable keys, with mustResolve set",
			source:      "${pod/foobar}${xyzzy}",
			mustResolve: true,
			result:      "",
			fail:        true,
		},
		{
			name:   "multiple literal $s",
			source: "$$$$$$",
			result: "$$$",
		},
		{
			name:   "trailing literal $",
			source: "foobar$$",
			result: "foobar$",
		},
		{
			name:   "trailing incorrect $",
			source: "foobar$",
			result: "",
			fail:   true,
		},
		{
			name:   "unterminated reference",
			source: "${foobar",
			result: "",
			fail:   true,
		},
		{
			name:   "unterminated reference",
			source: "${foobar$barfoo",
			result: "",
			fail:   true,
		},
		{
			name:   "unterminated reference",
			source: "${foobar",
			result: "",
			fail:   true,
		},
		{
			name:   "invalid reference, unexpected {",
			source: "${foo{bar}",
			result: "",
			fail:   true,
		},
		{
			name:   "invalid reference, unexpected }",
			source: "$foo}",
			result: "",
			fail:   true,
		},
		{
			name:   "non-variable curly braces",
			source: "{}$pod/name",
			result: "{}pod1",
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Expand(tc.source, ctr, tc.mustResolve)
			if !tc.fail {
				require.Equal(t, tc.result, result)
				require.Nil(t, err)
			} else {
				require.NotNil(t, err)
			}
		})
	}
}
