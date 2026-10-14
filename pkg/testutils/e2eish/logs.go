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

package e2eish

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	logcfg "github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log"
	"github.com/containers/nri-plugins/pkg/apis/config/v1alpha1/log/klogcontrol"
	logger "github.com/containers/nri-plugins/pkg/log"
)

var (
	// Log is the logger of the harness, for the steps of a test.
	Log = logger.NewLogger("e2eish")

	logDir    string
	logDirSet sync.Once
)

// captureLogs collects every log message of the test, debug ones included,
// and writes them to a file in $E2EISH_LOG_DIR if the test fails.
// E2EISH_VERBOSE=1 also prints them on the standard error as they come.
func captureLogs(t *testing.T) {
	t.Helper()

	buf := &bytes.Buffer{}
	var out io.Writer = buf
	if os.Getenv("E2EISH_VERBOSE") == "1" {
		out = io.MultiWriter(buf, os.Stderr)
	}
	old := logger.SetOutput(out)
	skipHeaders := true
	require.NoError(t, logger.Configure(&logcfg.Config{
		Debug:     []string{"*"},
		LogSource: true,
		Klog:      klogcontrol.Config{Skip_headers: &skipHeaders},
	}))
	Log.Infof("===== %s", t.Name())

	t.Cleanup(func() {
		logger.SetOutput(old)
		if err := logger.Configure(logger.ConfigFromEnv()); err != nil {
			t.Errorf("failed to restore logging: %v", err)
		}
		if !t.Failed() {
			return
		}

		logDirSet.Do(func() {
			if logDir = os.Getenv("E2EISH_LOG_DIR"); logDir == "" {
				logDir = filepath.Join(os.TempDir(), "e2eish",
					time.Now().Format("20060102-150405")+fmt.Sprintf("-%d", os.Getpid()))
			}
		})
		name := strings.ReplaceAll(t.Name(), "/", "_") + ".log"
		path := filepath.Join(logDir, name)
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			t.Errorf("failed to create log directory: %v", err)
			return
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Errorf("failed to write test log: %v", err)
			return
		}
		t.Logf("test log: %s", path)
	})
}
