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

import "testing"

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
