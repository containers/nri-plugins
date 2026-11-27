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

package kernel

import (
	"context"
	"time"
)

// poll returns nil as soon as done returns true, the error of done, or the
// error of the context when it ends first. done is checked at once, after
// every PollInterval, and at every change that Events tells.
func (k *Kernel) poll(ctx context.Context, done func() (bool, error)) error {
	for {
		var changed <-chan struct{}
		if k.Events != nil {
			// taken before the check: a change during the check wakes
			// the next one
			changed = k.Events.Changed()
		}
		ok, err := done()
		if err != nil || ok {
			return err
		}
		t := time.NewTimer(k.pollInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-changed:
		case <-t.C:
		}
		t.Stop()
	}
}

// sleep waits for d or until the context ends, and returns the error of
// the context then.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
