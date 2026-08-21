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
	"github.com/go-logr/logr"

	"github.com/containers/nri-plugins/pkg/log"
)

// newLogr returns a logr.Logger backed by the given pkg/log Logger.
// kubeletplugin.Start expects a logr.Logger injected into the context
// via logr.NewContext; this bridge avoids hand-rolling a LogSink.
func newLogr(l log.Logger) logr.Logger {
	return logr.FromSlogHandler(l.SlogHandler())
}
