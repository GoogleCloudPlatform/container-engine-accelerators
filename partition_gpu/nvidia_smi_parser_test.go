// Copyright 2022 Siemens AG. All Rights Reserved.
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
	"reflect"
	"strings"
	"testing"
)

var PROFILES_A30 = GPUAvailableProfiles{
	byname: map[string]GPUProfile{
		"1g.6gb": {
			id: 14,
			total: 4,
		},
		"1g.6gb+me": {
			id: 21,
			total: 1,
		},
		"2g.12gb": {
			id: 5,
			total: 2,
		},
		"4g.24gb": {
			id: 0,
			total: 1,
		},
	},
}

var SMIOUTPUT_A30 string = strings.TrimSpace(`
+-----------------------------------------------------------------------------+
| GPU instance profiles:                                                      |
| GPU   Name             ID    Instances   Memory     P2P    SM    DEC   ENC  |
|                              Free/Total   GiB              CE    JPEG  OFA  |
|=============================================================================|
|   0  MIG 1g.6gb        14     4/4        5.81       No     14     1     0   |
|                                                             1     0     0   |
+-----------------------------------------------------------------------------+
|   0  MIG 1g.6gb+me     21     1/1        5.81       No     14     1     0   |
|                                                             1     1     1   |
+-----------------------------------------------------------------------------+
|   0  MIG 2g.12gb        5     2/2        11.69      No     28     2     0   |
|                                                             2     0     0   |
+-----------------------------------------------------------------------------+
|   0  MIG 4g.24gb        0     1/1        23.44      No     56     4     0   |
|                                                             4     1     1   |
+-----------------------------------------------------------------------------+
`)

func Test_parseA30Config(t *testing.T) {
	got, err := ParseMIGAvailableProfiles(SMIOUTPUT_A30)
	if err != nil {
		t.Errorf("ParseMIGAvailableInstances() error = %v", err)
	}

	if len(got) != 1 {
		t.Errorf("ParseMIGAvailableInstances() len(res) = %v, expected = 1", len(got))
	}

	if !reflect.DeepEqual(got[0], PROFILES_A30) {
		t.Errorf("ParseMIGAvailableInstances() got = %v, expected = %v", got[0], PROFILES_A30)
	}
}
