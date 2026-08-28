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

package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8swatch "k8s.io/apimachinery/pkg/watch"
)

// Compile-time assertions: the re-exported aliases really are the same
// types and values as the upstream ones. These lines would fail to
// compile if the aliases drifted.
var (
	_ Interface = k8swatch.Interface(nil)
	_ EventType = k8swatch.EventType("")
	_ Event     = k8swatch.Event{}
)

func TestEventTypeConstants(t *testing.T) {
	tests := []struct {
		name  string
		local EventType
		want  k8swatch.EventType
	}{
		{"Added", Added, k8swatch.Added},
		{"Modified", Modified, k8swatch.Modified},
		{"Deleted", Deleted, k8swatch.Deleted},
		{"Bookmark", Bookmark, k8swatch.Bookmark},
		{"Error", Error, k8swatch.Error},
	}
	for _, tc := range tests {
		if tc.local != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.local, tc.want)
		}
	}
}

// TestObject_HappyPath confirms events sent to the fake Interface
// returned by CreateFn make it out through Object's ResultChan.
func TestObject_HappyPath(t *testing.T) {
	fake := k8swatch.NewFake()
	create := func(ctx context.Context, ns, name string) (Interface, error) {
		return fake, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ow, err := Object(ctx, "default", "cm-x", create)
	if err != nil {
		t.Fatalf("Object returned error: %v", err)
	}
	defer ow.Stop()

	// Push an Added and a Modified event through the fake; expect both.
	go func() {
		fake.Add(&metav1.Status{Message: "added"})
		fake.Modify(&metav1.Status{Message: "modified"})
	}()

	got := drainEvents(t, ow.ResultChan(), 2, 2*time.Second)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %+v", len(got), got)
	}
	if got[0].Type != Added {
		t.Errorf("event[0].Type = %q, want %q", got[0].Type, Added)
	}
	if got[1].Type != Modified {
		t.Errorf("event[1].Type = %q, want %q", got[1].Type, Modified)
	}
}

// TestObject_StopIdempotent verifies Stop() can be called more than
// once without panicking. The existing implementation uses sync.Once
// on the internal stop path; this is a defensive guard.
func TestObject_StopIdempotent(t *testing.T) {
	fake := k8swatch.NewFake()
	create := func(ctx context.Context, ns, name string) (Interface, error) {
		return fake, nil
	}

	ow, err := Object(t.Context(), "default", "cm-x", create)
	if err != nil {
		t.Fatalf("Object returned error: %v", err)
	}
	ow.Stop()
	ow.Stop() // must not panic
}

// TestFile_CreateVsWriteEventTypes is a defensive guard against
// regressing the Create-emits-Added / Write-emits-Modified distinction.
// PR #536's parallel implementation ships a copy-paste bug where both
// cases emit Added; we don't have that bug today, and this test
// ensures we don't accidentally introduce it in a future edit.
func TestFile_CreateVsWriteEventTypes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "watched.yaml")

	unmarshal := func(data []byte, name string) (runtime.Object, error) {
		return &metav1.Status{Message: string(data)}, nil
	}

	fw, err := File(file, unmarshal)
	if err != nil {
		t.Fatalf("File returned error: %v", err)
	}
	defer fw.Stop()

	// Step 1: create the file — expect an Added event from the fsnotify
	// Create.
	if err := os.WriteFile(file, []byte("v1"), 0o600); err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Step 2: modify the file using O_WRONLY|O_APPEND so fsnotify
	// emits only Write (not Create). os.WriteFile uses O_CREATE|O_TRUNC
	// which fires a Create event even for an existing file — that would
	// give us a second Added instead of the Modified we're testing for.
	time.Sleep(150 * time.Millisecond)
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.Write([]byte("+v2")); err != nil {
		_ = f.Close()
		t.Fatalf("append write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close after append: %v", err)
	}

	// Drain up to 4 events (initial-Added-from-run + Create-Added + Write-Modified + slack).
	got := drainEvents(t, fw.ResultChan(), 4, 3*time.Second)
	sawAdded, sawModified := false, false
	for _, ev := range got {
		if ev.Type == Added {
			sawAdded = true
		}
		if ev.Type == Modified {
			sawModified = true
		}
	}
	if !sawAdded {
		t.Errorf("expected at least one Added event; got types: %v", eventTypes(got))
	}
	if !sawModified {
		t.Errorf("expected at least one Modified event (write path); got types: %v", eventTypes(got))
	}
}

// drainEvents receives up to `want` events from ch or times out.
// Returns whatever it received.
func drainEvents(t *testing.T, ch <-chan Event, want int, timeout time.Duration) []Event {
	t.Helper()
	got := make([]Event, 0, want)
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			return got
		}
	}
	return got
}

func eventTypes(evs []Event) []EventType {
	out := make([]EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}
