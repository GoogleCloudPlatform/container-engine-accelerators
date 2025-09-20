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

package util

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

func TestDeviceNameFromPath(t *testing.T) {
	as := assert.New(t)
	name, err := DeviceNameFromPath("/dev/nvidia0")
	as.Nil(err)
	as.Equal("nvidia0", name)

	name, err = DeviceNameFromPath("/dev/somethingelse0")
	as.Error(err)
	as.Contains(err.Error(), "is not a valid GPU device path")
}

func TestWaitForDeviceUnregistered(t *testing.T) {
	errorEvent := watch.Event{
		Type: watch.Error,
		Object: &metav1.Status{
			Reason: "test error",
		},
	}

	tests := []struct {
		name     string
		event    watch.Event
		wantBool bool
		wantErr  error
	}{
		{
			name: "Modified event, GPU is 0",
			event: watch.Event{
				Type: watch.Modified,
				Object: &v1.Node{
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("0"),
						},
					},
				},
			},
			wantBool: true,
			wantErr:  nil,
		},
		{
			name: "Added event, GPU exists and is greater than 0",
			event: watch.Event{
				Type: watch.Added,
				Object: &v1.Node{
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
			wantBool: false,
			wantErr:  nil,
		},
		{
			name: "Modified event, GPU does not exist",
			event: watch.Event{
				Type: watch.Modified,
				Object: &v1.Node{
					Status: v1.NodeStatus{
						Allocatable: v1.ResourceList{},
					},
				},
			},
			wantBool: true,
			wantErr:  nil,
		},
		{
			name: "Deleted event",
			event: watch.Event{
				Type:   watch.Deleted,
				Object: &v1.Node{}, // Object doesn't matter for Deleted event
			},
			wantBool: true,
			wantErr:  fmt.Errorf("node deleted, exit here"),
		},
		{
			name:     "Error event",
			event:    errorEvent,
			wantBool: true,
			wantErr:  fmt.Errorf("node error received, exit here: %v", apierrors.FromObject(errorEvent.Object)),
		},
		{
			name: "Unexpected object type",
			event: watch.Event{
				Type:   watch.Modified,
				Object: &v1.Pod{}, // Not a v1.Node
			},
			wantBool: false,
			wantErr:  nil,
		},
		{
			name: "Unknown event type",
			event: watch.Event{
				Type:   watch.EventType("UNKNOWN"),
				Object: &v1.Node{},
			},
			wantBool: false,
			wantErr:  nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boolResult, err := WaitForDeviceUnregistered(test.event)

			if boolResult != test.wantBool {
				t.Errorf("WaitForDevicePluginShutdownCondition() gotBool = %v, want %v", boolResult, test.wantBool)
			}

			if (err != nil && test.wantErr == nil) || (err == nil && test.wantErr != nil) {
				t.Errorf("WaitForDevicePluginShutdownCondition() gotErr = %v, want %v", err, test.wantErr)
			} else if err != nil && test.wantErr != nil && err.Error() != test.wantErr.Error() {
				t.Errorf("WaitForDevicePluginShutdownCondition() gotErr = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestSafelyUsingFlockWait(t *testing.T) {
	const eventDuration = 50 * time.Millisecond
	const lockFileDuration = 100 * time.Millisecond

	tests := []struct {
		name            string
		locked          bool
		created         bool
		watchFuncCalled bool
		minTimeDuration time.Duration
		maxTimeDuration time.Duration
	}{
		{
			name:            "LockFileExistsLocked",
			locked:          true,
			created:         true,
			watchFuncCalled: false,
			minTimeDuration: 100 * time.Millisecond,
			maxTimeDuration: 105 * time.Millisecond,
		},
		{
			name:            "LockFileExistsNotLocked",
			locked:          false,
			created:         true,
			watchFuncCalled: false,
			minTimeDuration: 0,
			maxTimeDuration: 1 * time.Millisecond,
		},
		{
			name:            "LockFileNotExists",
			locked:          false,
			created:         false,
			watchFuncCalled: true,
			minTimeDuration: 50 * time.Millisecond,
			maxTimeDuration: 52 * time.Millisecond,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const lockFileName string = "file.lock"

			var fd int
			var duration time.Duration
			var funcErr error

			ctx := context.Background()

			tempDir, err := os.MkdirTemp("", "lockfile_test_")
			if err != nil {
				t.Fatalf("Failed to create temporary directory: %v", err)
			}
			defer os.RemoveAll(tempDir)
			lockFilePath := filepath.Join(tempDir, lockFileName)

			// Create lock file
			if tt.created {
				file, err := os.Create(lockFilePath)
				if err != nil {
					t.Fatalf("Failed to create dummy lock file: %v", err)
				}
				file.Close()
			}

			// Lock the file
			if tt.locked {
				fd, err = unix.Open(lockFilePath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0600)
				if err != nil {
					t.Errorf("Failed to open lock file: %v", err)
				}
				err = unix.Flock(fd, unix.LOCK_EX)
				if err != nil {
					t.Errorf("Failed to lock file: %v", err)
				}
			}
			mockCheckLockFileExists := func(string) (bool, error) {
				if tt.locked {
					time.Sleep(lockFileDuration)

					err = unix.Flock(fd, unix.LOCK_UN)
					if err != nil {
						t.Errorf("Failed to lock file: %v", err)
					}
					return true, nil
				}
				return false, nil
			}
			mockWathchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
				return nil, nil
			}

			mockUseRetryWatch := func(context.Context, func(options metav1.ListOptions) (watch.Interface, error), func(watch.Event) (bool, error)) error {
				if tt.watchFuncCalled {
					time.Sleep(eventDuration)
				}
				return nil
			}

			startTime := time.Now()

			funcErr = SafelyUsingFlockWait(ctx, lockFilePath, mockWathchFunc, mockCheckLockFileExists, mockUseRetryWatch)
			if funcErr != nil {
				t.Errorf("SafelyUsingFlockWait() returned unexpected error: %v", funcErr)
			}
			duration = time.Since(startTime)

			if duration < tt.minTimeDuration || duration > tt.maxTimeDuration {
				t.Errorf("Expected SafelyUsingFlockWait() to finish between %v and %v, but it took %v", tt.minTimeDuration, tt.maxTimeDuration, duration)
			}

			fd, err = unix.Open(lockFilePath, unix.O_RDWR|unix.O_CLOEXEC, 0600)
			if err != nil {
				t.Errorf("Failed to open lock file: %v", err)
			}
			err = unix.Flock(fd, unix.LOCK_NB|unix.LOCK_EX)
			if err != unix.EWOULDBLOCK {
				t.Errorf("Expected the file to be locked, but it is still lockable: %v", err)
			}
		})
	}
}
