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
	"path"
	"path/filepath"
	"strings"

	logger "github.com/containers/nri-plugins/pkg/log"
)

// Our logger instance.
var log = logger.NewLogger("expression")

// Validate checks the expression for (obvious) invalidity.
func (e *Expression) Validate() error {
	if e == nil {
		return exprError("nil expression")
	}

	if err := e.validateKey(); err != nil {
		return err
	}

	switch e.Op {
	case Equals, NotEqual:
		if len(e.Values) != 1 {
			return exprError("invalid expression, '%s' requires a single value", e.Op)
		}
	case Matches, MatchesNot:
		if len(e.Values) != 1 {
			return exprError("invalid expression, '%s' requires a single value", e.Op)
		}
	case Exists, NotExist:
		if e.Values != nil && len(e.Values) != 0 {
			return exprError("invalid expression, '%s' does not take any values", e.Op)
		}

	case In, NotIn:
	case MatchesAny, MatchesNone:

	case AlwaysTrue:
		if e.Values != nil && len(e.Values) != 0 {
			return exprError("invalid expression, '%s' does not take any values", e.Op)
		}

	default:
		return exprError("invalid expression, unknown operator: %q", e.Op)
	}
	return nil
}

func (e *Expression) validateKey() error {
	keys, _ := splitKeys(e.Key)

VALIDATE_KEYS:
	for _, key := range keys {
		key = strings.TrimLeft(key, "/")
		for {
			prefKey, restKey, _ := strings.Cut(key, "/")
			switch prefKey {
			case KeyID, KeyUID, KeyName, KeyNamespace, KeyQOSClass:
				if restKey != "" {
					return exprError("invalid expression, trailing key %q after %q",
						prefKey, restKey)
				}

			case KeyPod:
				if restKey == "" {
					return exprError("invalid expression, missing trailing pod key after %q",
						prefKey)
				}

			case KeyLabels:
				if restKey == "" {
					return exprError("invalid expression, missing trailing map key after %q",
						prefKey)
				}
				continue VALIDATE_KEYS // validate next key, assuming rest is label map key

			case KeyTags:
				if restKey == "" {
					return exprError("invalid expression, missing trailing map key after %q",
						prefKey)
				}
				continue VALIDATE_KEYS // validate next key, assuming rest is tag map key

			default:
				return exprError("invalid expression, unknown key %q", prefKey)
			}

			if restKey == "" {
				break
			}

			key = restKey
		}
	}

	return nil
}

// Evaluate evaluates an expression against a container.
func (e *Expression) Evaluate(subject Evaluable) bool {
	log.Debug("evaluating %q @ %s...", *e, subject)

	if e.Op == AlwaysTrue {
		return true
	}

	value, ok := KeyValue(e.Key, subject)
	result := false

	switch e.Op {
	case Equals:
		result = ok && (value == e.Values[0] || e.Values[0] == "*")
	case NotEqual:
		result = !ok || value != e.Values[0]
	case Matches, MatchesNot:
		match := false
		if ok {
			match, _ = filepath.Match(e.Values[0], value)
		}
		result = ok && match
		if e.Op == MatchesNot {
			result = !result
		}
	case In, NotIn:
		if ok {
			for _, v := range e.Values {
				if value == v || v == "*" {
					result = true
				}
			}
		}
		if e.Op == NotIn {
			result = !result
		}
	case MatchesAny, MatchesNone:
		if ok {
			for _, pattern := range e.Values {
				if match, _ := filepath.Match(pattern, value); match {
					result = true
					break
				}
			}
		}
		if e.Op == MatchesNone {
			result = !result
		}
	case Exists:
		result = ok
	case NotExist:
		result = !ok
	}

	log.Debug("%q @ %s => %v", *e, subject, result)

	return result
}

// String returns the expression as a string.
func (e *Expression) String() string {
	return fmt.Sprintf("<%s %s %s>", e.Key, e.Op, strings.Join(e.Values, ","))
}

// KeyValue extracts the value of the expression in the scope of the given subject.
func KeyValue(key string, subject Evaluable) (string, bool) {
	log.Debug("looking up %q @ %s...", key, subject)

	value := ""
	ok := false

	keys, vsep := splitKeys(key)
	if len(keys) == 1 {
		value, ok, _ = ResolveRef(subject, keys[0])
	} else {
		vals := make([]string, 0, len(keys))
		for _, key := range keys {
			v, found, _ := ResolveRef(subject, key)
			vals = append(vals, v)
			ok = ok || found
		}
		value = strings.Join(vals, vsep)
	}

	log.Debug("%q @ %s => %q, %v", key, subject, value, ok)

	return value, ok
}

func splitKeys(keys string) ([]string, string) {
	// We don't support boolean expressions but we support  'joint keys'.
	// These can be used to emulate a boolean AND of multiple keys.
	//
	// Joint keys have two valid forms:
	//   - ":keylist" (equivalent to ":::<colon-separated-keylist>")
	//   - ":<key-sep><value-sep><key-sep-separated-keylist>"
	//
	// The value of dereferencing such a key is the values of all individual
	// keys concatenated and separated by value-sep.

	if len(keys) < 4 || keys[0] != ':' {
		return []string{keys}, ""
	}

	keys = keys[1:]
	ksep := keys[0:1]
	vsep := keys[1:2]

	if validSeparator(ksep[0]) && validSeparator(vsep[0]) {
		keys = keys[2:]
	} else {
		ksep = ":"
		vsep = ":"
	}

	return strings.Split(keys, ksep), vsep
}

func validSeparator(b byte) bool {
	switch {
	case '0' <= b && b <= '9':
		return false
	case 'a' <= b && b <= 'z':
		return false
	case 'A' <= b && b <= 'Z':
		return false
	case b == '/', b == '.':
		return false
	}
	return true
}

// ResolveRef walks an object trying to resolve a reference to a value.
//
// Keys can be combined into compound keys using '/' as the separator.
// For instance, "pod/labels/io.test.domain/my-label" refers to the
// value of the "io.test.domain/my-label" label key of the pod of the
// evaluated object.
func ResolveRef(subject Evaluable, spec string) (string, bool, error) {
	var (
		key             = path.Clean(spec)
		obj interface{} = subject
	)

	log.Debug("resolving %q in %s...", key, subject)

	for {
		log.Debug("- resolve %q in %s", key, obj)

		switch v := obj.(type) {
		case Evaluable:
			pref, rest, _ := strings.Cut(key, "/")
			obj = v.EvalKey(pref)
			key = rest

		case map[string]string:
			value, ok := v[key]
			if !ok {
				return "", false, nil
			}
			obj = value
			key = ""

		case error:
			return "", false, exprError("%s: failed to resolve %q: %v", subject, spec, v)

		default:
			return "", false, exprError("%s: failed to resolve %q (%q): wrong type %T",
				subject, key, spec, v)
		}

		if key == "" {
			break
		}
	}

	s, ok := obj.(string)
	if !ok {
		return "", false, exprError("%s: failed to resolve %q: non-string type %T",
			subject, spec, obj)
	}

	log.Debug("resolved %q in %s => %s", spec, subject, s)

	return s, true, nil
}

// exprError returns a formatted error specific to expressions.
func exprError(format string, args ...interface{}) error {
	return fmt.Errorf("expression: "+format, args...)
}
