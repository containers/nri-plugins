// Copyright 2023 Intel Corporation. All Rights Reserved.
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

package main

import (
	policy "github.com/containers/nri-plugins/cmd/template/policy"
	logger "github.com/containers/nri-plugins/pkg/log"
	resmgr "github.com/containers/nri-plugins/pkg/resmgr/main"
)

var log = logger.Default()

func main() {
	resmgr, err := resmgr.New(policy.PolicyName)
	if err != nil {
		log.Fatalf("%v", err)
	}

	if err := resmgr.Run(); err != nil {
		log.Fatalf("%v", err)
	}
}