// Copyright 2017 Google Inc. All Rights Reserved.
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
	"testing"
)

func Test_buildPartitionStr(t *testing.T) {
	tests := []struct {
		name          string
		partitionSize string
		want          string
		wantErr       bool
	}{
		{
			name:          "Empty partition size",
			partitionSize: "",
			want:          "",
			wantErr:       false,
		},
		{
			name:          "Single partition",
			partitionSize: "7g.40gb",
			want:          "0",
			wantErr:       false,
		},
		{
			name:          "Invalid partition",
			partitionSize: "8g.40gb",
			want:          "",
			wantErr:       true,
		},
		{
			name:          "Two partitions",
			partitionSize: "3g.20gb",
			want:          "9,9",
			wantErr:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildPartitionStr(tt.partitionSize)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildPartitionStr() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("buildPartitionStr() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_parseLGIOutput(t *testing.T) {
	tests := []struct {
		name        string
		lgiOutput   string
		wantMap     map[string][]string
		wantUniform bool
	}{
		{
			name:        "Empty input",
			lgiOutput:   "",
			wantMap:     make(map[string][]string),
			wantUniform: false,
		},
		{
			name: "Header and footer only",
			lgiOutput: `
+-----------------------------------------------------------------------------+
| GPU instances:                                                              |
| GPU   Profile Name   Profile ID   CI_ID   Address                           |
|=============================================================================|
+-----------------------------------------------------------------------------+
			`,
			wantMap:     make(map[string][]string),
			wantUniform: false,
		},
		{
			name: "Single GPU, single GI, uniform",
			lgiOutput: `
+-----------------------------------------------------------------------------+
| GPU   Profile Name   Profile ID   CI_ID   Address                           |
|=============================================================================|
|   0   MIG 1g.5gb     19           0       00000000                          |
+-----------------------------------------------------------------------------+
			`,
			wantMap:     map[string][]string{"0": {"19"}},
			wantUniform: true,
		},
		{
			name: "Single GPU, multiple GIs, uniform",
			lgiOutput: `
+-----------------------------------------------------------------------------+
| GPU   Profile Name   Profile ID   CI_ID   Address                           |
|=============================================================================|
|   0   MIG 1g.5gb     19           0       00000000                          |
|   0   MIG 1g.5gb     19           1       00000001                          |
+-----------------------------------------------------------------------------+
			`,
			wantMap:     map[string][]string{"0": {"19", "19"}},
			wantUniform: true,
		},
		{
			name: "Single GPU, multiple GIs, non-uniform profile IDs",
			lgiOutput: `
+-----------------------------------------------------------------------------+
| GPU   Profile Name   Profile ID   CI_ID   Address                           |
|=============================================================================|
|   0   MIG 1g.5gb     19           0       00000000                          |
|   0   MIG 2g.10gb    14           0       01000000                          |
+-----------------------------------------------------------------------------+
			`,
			wantMap:     map[string][]string{"0": {"19"}},
			wantUniform: false,
		},
		{
			name: "Multiple GPUs, multiple GIs, all uniform",
			lgiOutput: `
+-----------------------------------------------------------------------------+
| GPU   Profile Name   Profile ID   CI_ID   Address                           |
|=============================================================================|
|   0   MIG 1g.5gb     19           0       00000000                          |
|   0   MIG 1g.5gb     19           1       01000000                          |
|   1   MIG 1g.5gb     19           0       00000000                          |
|   1   MIG 1g.5gb     19           1       01000000                          |
+-----------------------------------------------------------------------------+
			`,
			wantMap:     map[string][]string{"0": {"19", "19"}, "1": {"19", "19"}},
			wantUniform: true,
		},
		{
			name: "Multiple GPUs, non-uniform profile IDs between GPUs",
			lgiOutput: `
+-----------------------------------------------------------------------------+
| GPU   Profile Name   Profile ID   CI_ID   Address                           |
|=============================================================================|
|   0   MIG 1g.5gb     19           0       00000000                          |
|   1   MIG 2g.10gb    14           0       00000000                          |
+-----------------------------------------------------------------------------+
			`,
			wantMap:     map[string][]string{"0": {"19"}},
			wantUniform: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMap, gotUniform, _ := parseLGIOutput(tt.lgiOutput)
			if !reflect.DeepEqual(gotMap, tt.wantMap) {
				t.Errorf("parseLGIOutput() gotMap = %v, want %v", gotMap, tt.wantMap)
			}
			if gotUniform != tt.wantUniform {
				t.Errorf("parseLGIOutput() gotUniform = %v, want %v", gotUniform, tt.wantUniform)
			}
		})
	}
}

func Test_checkDesired(t *testing.T) {
	tests := []struct {
		name            string
		partitions      map[string][]string
		desiredMaxCount int
		want            bool
	}{
		{name: "Empty partitions map", partitions: make(map[string][]string), desiredMaxCount: 2, want: false},
		{name: "Single GPU, count matches", partitions: map[string][]string{"0": {"19", "19"}}, desiredMaxCount: 2, want: true},
		{name: "Single GPU, count less than desired", partitions: map[string][]string{"0": {"19"}}, desiredMaxCount: 2, want: false},
		{name: "Single GPU, count more than desired", partitions: map[string][]string{"0": {"19", "19", "19"}}, desiredMaxCount: 2, want: false},
		{name: "Multiple GPUs, all counts match", partitions: map[string][]string{"0": {"19", "19"}, "1": {"14", "14"}}, desiredMaxCount: 2, want: true},
		{name: "Multiple GPUs, first GPU matches, second does not", partitions: map[string][]string{"0": {"19", "19"}, "1": {"14"}}, desiredMaxCount: 2, want: false},
		{name: "Multiple GPUs, first GPU does not match, second matches", partitions: map[string][]string{"0": {"19"}, "1": {"14", "14"}}, desiredMaxCount: 2, want: false},
		{name: "Multiple GPUs, no counts match", partitions: map[string][]string{"0": {"19"}, "1": {"14"}}, desiredMaxCount: 2, want: false},
		{name: "Partitions map has GPUs, but desired count is 0 (should be false as GPUs exist)", partitions: map[string][]string{"0": {}}, desiredMaxCount: 0, want: true},
		{name: "Partitions map has GPUs with items, desired count is 0", partitions: map[string][]string{"0": {"19"}}, desiredMaxCount: 0, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkDesired(tt.partitions, tt.desiredMaxCount); got != tt.want {
				t.Errorf("checkDesired() = %v, want %v for partitions %v, desiredMaxCount %d", got, tt.want, tt.partitions, tt.desiredMaxCount)
			}
		})
	}
}
