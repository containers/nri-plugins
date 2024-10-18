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
	"path"
	"sync"
	"time"

	k8swatch "k8s.io/apimachinery/pkg/watch"
)

const (
	createDelay = 1 * time.Second
)

type watch struct {
	sync.Mutex
	subject string
	client  ObjectClient
	w       Interface
	resultC chan Event
	createC <-chan time.Time

	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// ObjectClient takes care of low-level details of creating a wrapped watch.
type ObjectClient interface {
	CreateWatch() (Interface, error)
}

// Object creates a wrapped watch using the given client. The watch
// is transparently recreated upon expiration and any errors.
func Object(client ObjectClient, subject string) (Interface, error) {
	w := &watch{
		subject: subject,
		client:  client,
		resultC: make(chan Event, k8swatch.DefaultChanSize),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}

	if err := w.create(); err != nil {
		return nil, err
	}

	go w.run()

	return w, nil
}

// Stop stops the watch.
func (w *watch) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopC)
		_ = <-w.doneC
		w.stop()
		w.stopC = nil
	})
}

// ResultChan returns the channel for receiving events from the watch.
func (w *watch) ResultChan() <-chan Event {
	return w.resultC
}

func (w *watch) eventChan() <-chan Event {
	if w.w == nil {
		return nil
	}

	return w.w.ResultChan()
}

func (w *watch) run() {
	for {
		select {
		case <-w.stopC:
			close(w.resultC)
			close(w.doneC)
			return

		case e, ok := <-w.eventChan():
			if !ok {
				log.Debug("%s failed...", w.name())
				w.stop()
				w.scheduleCreate()
				continue
			}

			if e.Type == Error {
				log.Debug("%s failed with an error...", w.name())
				w.stop()
				w.scheduleCreate()
				continue
			}

			select {
			case w.resultC <- e:
			default:
				log.Debug("%s failed to deliver %v event", w.name(), e.Type)
				w.stop()
				w.scheduleCreate()
			}

		case <-w.createC:
			w.createC = nil
			log.Debug("reopening %s...", w.name())
			if err := w.create(); err != nil {
				w.scheduleCreate()
			}
		}
	}
}

func (w *watch) create() error {
	w.stop()

	wif, err := w.client.CreateWatch()
	if err != nil {
		return err
	}
	w.w = wif

	return nil
}

func (w *watch) scheduleCreate() {
	if w.createC == nil {
		w.createC = time.After(createDelay)
	}
}

func (w *watch) stop() {
	if w.w == nil {
		return
	}

	w.w.Stop()
	w.w = nil
}

func (w *watch) name() string {
	return path.Join("watch", w.subject)
}
