// Copyright 2019 Intel Corporation. All Rights Reserved.
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

package utils

import (
	"errors"
	"net"
	"os"
	"syscall"
)

// IsListeningSocket returns true if connections are accepted on the socket.
func IsListeningSocket(socket string) (bool, error) {
	conn, err := net.Dial("unix", socket)
	if err == nil {
		conn.Close()
		return true, nil
	}

	if errors.Is(err, syscall.ECONNREFUSED) || os.IsNotExist(err) {
		return false, nil
	}

	return false, err
}
