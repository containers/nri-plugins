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

package api

import (
	"errors"
	"fmt"
	"net/http"
)

// Error codes.
const (
	CodeNotFound        = "NotFound"
	CodeConflict        = "Conflict"
	CodeInvalidArgument = "InvalidArgument"
	CodeUnavailable     = "Unavailable"
	CodeInternal        = "Internal"
)

// Error is the JSON body of every non-2xx response. As a Go error it
// carries the error code, so that handlers and clients can map it to
// HTTP status codes and exit codes.
type Error struct {
	Message string `json:"error"`
	Code    string `json:"code"`
	// Object, if set, is the current state of the object the request
	// was about (for instance an Attachment that stays "detaching").
	Object any `json:"object,omitempty"`
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// HTTPStatus returns the HTTP status code that matches the error code.
func (e *Error) HTTPStatus() int {
	return HTTPStatusForCode(e.Code)
}

// HTTPStatusForCode maps an error code to an HTTP status code.
func HTTPStatusForCode(code string) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// CodeForHTTPStatus maps an HTTP status code to an error code.
func CodeForHTTPStatus(status int) string {
	switch status {
	case http.StatusNotFound:
		return CodeNotFound
	case http.StatusConflict:
		return CodeConflict
	case http.StatusBadRequest:
		return CodeInvalidArgument
	case http.StatusServiceUnavailable:
		return CodeUnavailable
	default:
		return CodeInternal
	}
}

func newError(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// NotFound returns a NotFound error.
func NotFound(format string, args ...any) *Error {
	return newError(CodeNotFound, format, args...)
}

// Conflict returns a Conflict error.
func Conflict(format string, args ...any) *Error {
	return newError(CodeConflict, format, args...)
}

// InvalidArgument returns an InvalidArgument error.
func InvalidArgument(format string, args ...any) *Error {
	return newError(CodeInvalidArgument, format, args...)
}

// Unavailable returns an Unavailable error.
func Unavailable(format string, args ...any) *Error {
	return newError(CodeUnavailable, format, args...)
}

// Internal returns an Internal error.
func Internal(format string, args ...any) *Error {
	return newError(CodeInternal, format, args...)
}

// ErrorCode returns the API error code of err, CodeInternal if err is not
// an *Error, "" if err is nil.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return CodeInternal
}

// IsCode returns true if err is an *Error with the code.
func IsCode(err error, code string) bool {
	return err != nil && ErrorCode(err) == code
}
