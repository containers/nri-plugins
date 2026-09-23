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

package cxl

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

type parser struct {
	fileCache     map[string]string
	ignoredErrors []error
}

type parseFunc func(p *parser) error

func parseWithSscanf(filepath, format string, dests ...any) parseFunc {
	return func(p *parser) error {
		data, err := p.getFileContent(filepath)
		if err != nil {
			return err
		}
		for line := range strings.SplitSeq(data, "\n") {
			if _, err := fmt.Sscanf(line, format, dests...); err == nil {
				return nil
			}
		}
		return fmt.Errorf("%w: no line in %q matches format %q", ErrMissingLine, filepath, format)
	}
}

// parseIgnoring makes the parse tasks that follow it tolerate any error matching
// one of targets. It replaces the set of ignored errors rather than adding to it;
// parseIgnoring() with no targets restores the default of ignoring nothing.
func parseIgnoring(targets ...error) parseFunc {
	ignored := slices.Clone(targets)
	return func(p *parser) error {
		p.ignoredErrors = ignored
		return nil
	}
}

func (p *parser) getFileContent(filepath string) (string, error) {
	data, ok := p.fileCache[filepath]
	if !ok {
		dataBytes, err := os.ReadFile(filepath)
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: %w", ErrMissingFile, err)
		}
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrUnreadableFile, err)
		}
		data = strings.TrimSpace(string(dataBytes))
		p.fileCache[filepath] = data
	}
	return data, nil
}

func (p *parser) parse(parseTasks ...parseFunc) error {
	for _, pt := range parseTasks {
		if err := pt(p); err != nil {
			if p.isIgnored(err) {
				continue
			}
			return err
		}
	}
	return nil
}

func (p *parser) isIgnored(err error) bool {
	for _, target := range p.ignoredErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func parse(parseTasks ...parseFunc) error {
	p := &parser{
		fileCache: map[string]string{},
	}
	return p.parse(parseTasks...)
}
