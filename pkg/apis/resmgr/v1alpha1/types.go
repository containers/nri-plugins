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

package resmgr

// Expression describes some runtime-evaluated condition. An expression
// consist of a key, an operator and a set of values. An expressions is
// evaluated against an object which implements the Evaluable interface.
// Evaluating an expression consists of looking up the value for the key
// in the object, then using the operator to check it agains the values
// of the expression. The result is a single boolean value. An object is
// said to satisfy the evaluated expression if this value is true. An
// expression can contain 0, 1 or more values depending on the operator.
// +k8s:deepcopy-gen=true
type Expression struct {
	// Key is the expression key.
	Key string `json:"key"`
	// Op is the expression operator.
	// +kubebuilder:validation:Enum=Equals;NotEqual;In;NotIn;Exists;NotExist;AlwaysTrue;Matches;MatchesNot;MatchesAny;MatchesNone
	// +kubebuilder:validation:Format:string
	Op Operator `json:"operator"`
	// Values contains the values the key value is evaluated against.
	Values []string `json:"values,omitempty"`
}

// Operator is an expression operator.
type Operator string

// supported operators
const (
	// Equals tests for equality with a single value.
	Equals Operator = "Equals"
	// NotEqual test for inequality with a single value.
	NotEqual Operator = "NotEqual"
	// In tests for any value for the given set.
	In Operator = "In"
	// NotIn tests for the lack of value in a given set.
	NotIn Operator = "NotIn"
	// Exists tests if the given key exists with any value.
	Exists Operator = "Exists"
	// NotExist tests if the given key does not exist.
	NotExist Operator = "NotExist"
	// AlwaysTrue always evaluates to true.
	AlwaysTrue Operator = "AlwaysTrue"
	// Matches tests if the key value matches a single globbing pattern.
	Matches Operator = "Matches"
	// MatchesNot tests if the key value does not match a single globbing pattern.
	MatchesNot Operator = "MatchesNot"
	// MatchesAny tests if the key value matches any of a set of globbing patterns.
	MatchesAny Operator = "MatchesAny"
	// MatchesNone tests if the key value matches none of a set of globbing patterns.
	MatchesNone Operator = "MatchesNone"
)

// Keys of supported object properties.
const (
	// Pod of the object.
	KeyPod = "pod"
	// ID of the object.
	KeyID = "id"
	// UID of the object.
	KeyUID = "uid"
	// Name of the object.
	KeyName = "name"
	// Namespace of the object.
	KeyNamespace = "namespace"
	// QoSClass of the object.
	KeyQOSClass = "qosclass"
	// Labels of the object.
	KeyLabels = "labels"
	// Tags of the object.
	KeyTags = "tags"
)
