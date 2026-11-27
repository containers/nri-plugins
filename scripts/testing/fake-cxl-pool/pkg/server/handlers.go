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

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/containers/nri-plugins/scripts/testing/fake-cxl-pool/pkg/api"
)

// Handler returns the HTTP handler of the REST API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(api.RouteStatus, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Status())
	})
	mux.HandleFunc(api.RouteHosts, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Hosts(r.Context()))
	})
	mux.HandleFunc(api.RouteHostsResolve, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		h, err := s.Resolve(q.Get("hostname"), q.Get("uuid"))
		reply(w, http.StatusOK, h, err)
	})
	mux.HandleFunc(api.RouteHost, func(w http.ResponseWriter, r *http.Request) {
		h, err := s.Host(r.Context(), r.PathValue("name"))
		reply(w, http.StatusOK, h, err)
	})
	mux.HandleFunc(api.RouteDevices, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.Devices(r.URL.Query().Get("scope")))
	})
	mux.HandleFunc(api.RouteDevicesCreate, func(w http.ResponseWriter, r *http.Request) {
		var req api.DeviceCreate
		if !decodeJSON(w, r, &req) {
			return
		}
		d, err := s.CreateDevice(req)
		reply(w, http.StatusCreated, d, err)
	})
	mux.HandleFunc(api.RouteDevice, func(w http.ResponseWriter, r *http.Request) {
		d, err := s.Device(r.PathValue("name"))
		reply(w, http.StatusOK, d, err)
	})
	mux.HandleFunc(api.RouteDeviceDelete, func(w http.ResponseWriter, r *http.Request) {
		err := s.DeleteDevice(r.Context(), r.PathValue("name"), boolQuery(r, "force"))
		reply(w, http.StatusNoContent, nil, err)
	})
	mux.HandleFunc(api.RouteDeviceAttach, func(w http.ResponseWriter, r *http.Request) {
		var req api.AttachRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		a, res, err := s.Attach(r.Context(), r.PathValue("name"), req)
		status := http.StatusOK
		switch res {
		case AttachCreated:
			status = http.StatusCreated
		case AttachAccepted:
			status = http.StatusAccepted
		}
		reply(w, status, a, err)
	})
	mux.HandleFunc(api.RouteDeviceDetach, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		opts := DetachOptions{Force: boolQuery(r, "force")}
		if v := q.Get("timeout"); v != "" {
			t, err := time.ParseDuration(v)
			if err != nil {
				writeError(w, api.InvalidArgument("invalid timeout=%q", v))
				return
			}
			opts.Timeout = t
		}
		a, res, err := s.Detach(r.Context(), r.PathValue("name"), r.PathValue("host"), opts)
		status := http.StatusOK
		if res == DetachAccepted {
			status = http.StatusAccepted
		}
		reply(w, status, a, err)
	})
	mux.HandleFunc(api.RouteAttachments, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		as, err := s.Attachments(AttachmentFilter{Host: q.Get("host"), Device: q.Get("device")})
		reply(w, http.StatusOK, as, err)
	})
	mux.HandleFunc(api.RouteEvents, s.handleEvents)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, api.NotFound("no route %s %s", r.Method, r.URL.Path))
	})
	return s.logRequests(mux)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, api.Internal("streaming not supported"))
		return
	}
	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprintf(w, ": fake-cxl-pool events\n\n"); err != nil {
		return
	}
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-ping.C:
			_, err = fmt.Fprintf(w, ": ping\n\n")
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, merr := json.Marshal(ev)
			if merr != nil {
				continue
			}
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
		}
		if err != nil {
			return
		}
		fl.Flush()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if s.opts.Verbose || (r.Method != http.MethodGet && r.URL.Path != api.PathEvents()) {
			s.logf("http %s %s %s -> %d (%s)", r.RemoteAddr, r.Method, r.URL.RequestURI(), rec.status, time.Since(start).Round(time.Millisecond))
		}
	})
}

func boolQuery(r *http.Request, key string) bool {
	b, _ := strconv.ParseBool(r.URL.Query().Get(key))
	return b
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, api.InvalidArgument("cannot read request body: %v", err))
		return false
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, api.InvalidArgument("invalid request body: %v", err))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	e, ok := errors.AsType[*api.Error](err)
	if !ok {
		e = &api.Error{Code: api.CodeInternal, Message: err.Error()}
	}
	writeJSON(w, e.HTTPStatus(), e)
}
