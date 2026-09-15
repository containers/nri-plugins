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

// Package coverage serves the coverage data of a plugin built with coverage
// instrumentation, using make COVER=1, over the instrumentation HTTP server.
// It lets tests which run a plugin as a whole, in a cluster, collect coverage
// data from it the way go test -cover does for unit tests. Our e2e tests use
// this to take a snapshot of the counters per test case.
//
// Save the served data to files named covmeta.<id> and covcounters.<id>.N.M,
// with <id> from the ID endpoint, and go tool covdata can digest the result.
// A plugin built without instrumentation serves an error for all endpoints.
package coverage

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"runtime/coverage"
	"sync"

	xhttp "github.com/containers/nri-plugins/pkg/http"
	logger "github.com/containers/nri-plugins/pkg/log"
)

const (
	// IDPath serves the ID of the instrumented binary.
	IDPath = "/coverage/id"
	// MetaPath serves the coverage meta-data of the binary.
	MetaPath = "/coverage/meta"
	// CountersPath serves a snapshot of the coverage counters.
	CountersPath = "/coverage/counters"
	// ClearPath resets the coverage counters.
	ClearPath = "/coverage/clear"
)

// Layout of the leading fields of the coverage meta-data, as emitted by
// runtime/coverage and declared by internal/coverage.MetaFileHeader. All we
// need from it is the hash which identifies the instrumented binary, and
// which the go coverage tooling expects to find in the names of the files
// the data is stored in. Check the magic and version to fail loudly instead
// of serving a bogus ID, in case the layout ever changes.
const (
	metaMagicLen   = 4
	metaVersionLen = 4
	metaHashOffset = 24
	metaHashLen    = 16
	metaHeaderLen  = metaHashOffset + metaHashLen
)

var (
	metaMagic   = [metaMagicLen]byte{0x00, 0x63, 0x76, 0x6d}
	metaVersion = uint32(1)
)

var (
	log = logger.NewLogger("coverage")

	idOnce sync.Once
	id     string
	idErr  error
)

// Setup prepares the given HTTP request multiplexer for serving coverage data.
func Setup(mux *xhttp.ServeMux) {
	if id, err := binaryID(); err != nil {
		log.Warnf("coverage data is not available: %v", err)
	} else {
		log.Infof("serving coverage data of instrumented binary %s", id)
	}

	mux.HandleFunc(IDPath, serveID)
	mux.HandleFunc(MetaPath, serveMeta)
	mux.HandleFunc(CountersPath, serveCounters)
	mux.HandleFunc(ClearPath, clearCounters)
}

// serveID serves the ID of the instrumented binary.
func serveID(w http.ResponseWriter, req *http.Request) {
	id, err := binaryID()
	if err != nil {
		serveError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	writeResponse(w, []byte(id))
}

// serveMeta serves the coverage meta-data of the binary.
func serveMeta(w http.ResponseWriter, req *http.Request) {
	serveData(w, coverage.WriteMeta)
}

// serveCounters serves a snapshot of the coverage counters.
func serveCounters(w http.ResponseWriter, req *http.Request) {
	serveData(w, coverage.WriteCounters)
}

// clearCounters resets the coverage counters. This allows a test to collect
// the coverage of a single test case, instead of everything since startup.
func clearCounters(w http.ResponseWriter, req *http.Request) {
	if err := coverage.ClearCounters(); err != nil {
		serveError(w, err)
		return
	}

	log.Infof("coverage counters cleared")

	w.Header().Set("Content-Type", "text/plain")
	writeResponse(w, []byte("ok"))
}

// serveData serves the data produced by the given coverage dump function.
// The data is buffered so that a failed dump is reported as an error, instead
// of as a partial and silently unusable response.
func serveData(w http.ResponseWriter, dump func(io.Writer) error) {
	buf := &bytes.Buffer{}

	if err := dump(buf); err != nil {
		serveError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	writeResponse(w, buf.Bytes())
}

// serveError fails a request with the given error.
func serveError(w http.ResponseWriter, err error) {
	log.Errorf("failed to serve coverage data: %v", err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// writeResponse writes a successful response.
func writeResponse(w http.ResponseWriter, data []byte) {
	if _, err := w.Write(data); err != nil {
		log.Errorf("failed to write response: %v", err)
	}
}

// binaryID returns the ID of this instrumented binary, digging it out of the
// header of the coverage meta-data. It fails if the binary was built without
// coverage instrumentation.
func binaryID() (string, error) {
	idOnce.Do(func() {
		hdr := &metaHeader{}
		if err := coverage.WriteMeta(hdr); err != nil {
			idErr = fmt.Errorf("failed to read coverage meta-data: %w", err)
			return
		}
		id, idErr = hdr.binaryID()
	})

	return id, idErr
}

// metaHeader collects the header of the coverage meta-data, discarding the
// rest of it.
type metaHeader struct {
	data []byte
}

// Write collects data until we have the full header.
func (m *metaHeader) Write(b []byte) (int, error) {
	if need := metaHeaderLen - len(m.data); need > 0 {
		m.data = append(m.data, b[:min(need, len(b))]...)
	}
	return len(b), nil
}

// binaryID extracts the binary ID from the collected header.
func (m *metaHeader) binaryID() (string, error) {
	if len(m.data) < metaHeaderLen {
		return "", fmt.Errorf("coverage meta-data short by %d bytes",
			metaHeaderLen-len(m.data))
	}

	if magic := [metaMagicLen]byte(m.data[:metaMagicLen]); magic != metaMagic {
		return "", fmt.Errorf("unexpected coverage meta-data magic %#x, expected %#x",
			magic, metaMagic)
	}

	version := binary.LittleEndian.Uint32(m.data[metaMagicLen : metaMagicLen+metaVersionLen])
	if version != metaVersion {
		return "", fmt.Errorf("unexpected coverage meta-data version %d, expected %d",
			version, metaVersion)
	}

	return hex.EncodeToString(m.data[metaHashOffset:metaHeaderLen]), nil
}
