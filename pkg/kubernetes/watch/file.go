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
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8swatch "k8s.io/apimachinery/pkg/watch"
)

// FileClient takes care of unmarshaling file content to a target object.
type FileClient interface {
	Unmarshal([]byte, string) (runtime.Object, error)
}

type fileWatch struct {
	dir      string
	file     string
	client   FileClient
	fsw      *fsnotify.Watcher
	resultC  chan Event
	stopOnce sync.Once
	stopC    chan struct{}
	doneC    chan struct{}
}

// File creates a k8s-compatible watch for the given file. Contents of the
// file are unmarshalled into the object of choice by the client. The file
// is monitored for changes and corresponding watch events are generated.
func File(client FileClient, file string) (Interface, error) {
	absPath, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err = fsw.Add(filepath.Dir(absPath)); err != nil {
		return nil, err
	}

	fw := &fileWatch{
		dir:     filepath.Dir(absPath),
		file:    filepath.Base(absPath),
		client:  client,
		fsw:     fsw,
		resultC: make(chan Event, k8swatch.DefaultChanSize),
		stopC:   make(chan struct{}),
		doneC:   make(chan struct{}),
	}

	obj, err := fw.readObject()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	fw.sendEvent(Added, obj)

	go fw.run()

	return fw, nil
}

// Stop stops the watch.
func (w *fileWatch) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopC)
		<-w.doneC
		w.stopC = nil
	})
}

// ResultChan returns the channel for receiving events from the watch.
func (w *fileWatch) ResultChan() <-chan Event {
	return w.resultC
}

func (w *fileWatch) run() {
	for {
		select {
		case <-w.stopC:
			if err := w.fsw.Close(); err != nil {
				log.Warn("%s failed to close fsnotify watcher: %v", w.name(), err)
			}
			close(w.resultC)
			close(w.doneC)
			return

		case e, ok := <-w.fsw.Events:
			log.Debug("%s got event %+v", w.name(), e)

			if !ok {
				w.sendEvent(
					Error,
					&metav1.Status{
						Status:  metav1.StatusFailure,
						Message: "failed to receive fsnotify event",
					},
				)
				close(w.resultC)
				close(w.doneC)
				return
			}

			if path.Base(e.Name) != w.file {
				continue
			}

			switch {
			case (e.Op & (fsnotify.Create | fsnotify.Write)) != 0:
				obj, err := w.readObject()
				if err != nil {
					log.Debug("%s failed to read/unmarshal: %v", w.name(), err)
					continue
				}

				if (e.Op & fsnotify.Create) != 0 {
					w.sendEvent(Added, obj)
				} else {
					w.sendEvent(Added, obj)
				}

			case (e.Op & (fsnotify.Remove | fsnotify.Rename)) != 0:
				w.sendEvent(Deleted, nil)
			}
		}
	}
}

func (w *fileWatch) sendEvent(t EventType, obj runtime.Object) {
	select {
	case w.resultC <- Event{Type: t, Object: obj}:
	default:
		log.Warn("failed to deliver fileWatch %v event", t)
	}
}

func (w *fileWatch) readObject() (runtime.Object, error) {
	file := path.Join(w.dir, w.file)
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}

	obj, err := w.client.Unmarshal(data, file)
	if err != nil {
		return nil, err
	}

	log.Debug("%s read object %+v", w.name(), obj)

	return obj, nil
}

func (w *fileWatch) name() string {
	return path.Join("filewatch/path:", path.Join(w.dir, w.file))
}
