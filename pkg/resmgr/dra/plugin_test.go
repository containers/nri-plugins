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

package dra

import (
	"errors"
	"testing"
)

func TestNew_ReturnsNotImplemented(t *testing.T) {
	p, err := New("test-driver", Deps{})
	if p != nil {
		t.Errorf("New() returned non-nil Plugin, want nil")
	}
	if err == nil {
		t.Fatal("New() returned nil error, want non-nil")
	}
	if !errors.Is(err, errNotImplemented) {
		t.Errorf("New() error = %v, want errors.Is(err, errNotImplemented) == true", err)
	}
}
