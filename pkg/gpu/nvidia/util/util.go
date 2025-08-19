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
	"regexp"

	"github.com/fsnotify/fsnotify"
	"github.com/golang/glog"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	retryWatch "k8s.io/client-go/tools/watch"
)

const ()

func DeviceNameFromPath(path string) (string, error) {
	gpuPathRegex := regexp.MustCompile("/dev/(nvidia[0-9]+)$")
	m := gpuPathRegex.FindStringSubmatch(path)
	if len(m) != 2 {
		return "", fmt.Errorf("path (%s) is not a valid GPU device path", path)
	}
	return m[1], nil
}

// Files creates a Watcher for the specified files.
func Files(files ...string) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	for _, f := range files {
		err = watcher.Add(f)
		if err != nil {
			watcher.Close()
			return nil, err
		}
	}
	return watcher, nil
}

func BuildKubeClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		glog.Errorf("failed to get kube config. Error: %v", err)
		return nil, err
	}
	config.ContentType = runtime.ContentTypeProtobuf

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Errorf("failed to get kube client. Error: %v", err)
		return nil, err
	}

	return kubeClient, nil
}

func GetEnv(envName string) (string, error) {
	env := os.Getenv(envName)
	if len(env) == 0 {
		return "", fmt.Errorf("empty %s environment variable", envName)
	}
	return env, nil
}

func CheckLockFileExists(lockFilePath string) (bool, error) {
	if _, err := os.Stat(lockFilePath); err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else {
		return false, err
	}
}

// Function containing the blocking logic that processes node events
func WaitForDeviceUnregistered(event watch.Event) (bool, error) {
	if event.Type == watch.Modified || event.Type == watch.Added {
		node, ok := event.Object.(*v1.Node)
		if !ok {
			glog.Warningf("unexpected object type: %T", event.Object)
			return false, nil
		}

		tpuQuantity, exists := node.Status.Allocatable["nvidia.com/gpu"]
		if !exists || tpuQuantity.Value() == 0 {
			glog.Infoln("nvidia.com/gpu is 0. Proceeding to critical section.")
			return true, nil
		}
		glog.Infoln("Waiting for nvidia.com/gpu to be 0...", tpuQuantity.Value())
		return false, nil
	}
	if event.Type == watch.Deleted {
		return true, fmt.Errorf("node deleted, exit here")
	}
	if event.Type == watch.Error {
		return true, fmt.Errorf("node error received, exit here: %v", apierrors.FromObject(event.Object))
	}
	return false, nil
}

// Copyied from k8s.io/kubernetes/pkg/util/flock
// Acquire acquires a lock on a file for the duration of the process. This method
// is reentrant.
func Acquire(path string) error {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}

	// We don't need to close the fd since we should hold
	// it until the process exits.

	return unix.Flock(fd, unix.LOCK_EX)
}

func UseRetryWatch(ctx context.Context, watchFunc func(metav1.ListOptions) (watch.Interface, error), conditions func(watch.Event) (bool, error)) error {
	_, err := retryWatch.Until(ctx, "1", &cache.ListWatch{WatchFunc: watchFunc}, conditions)
	if err != nil {
		return fmt.Errorf("failed to wait for device unregistered: %v", err)
	}
	return nil
}

func SafelyUsingFlockWait(
	ctx context.Context,
	lockFilePath string,
	watchFunc func(metav1.ListOptions) (watch.Interface, error),
	checkLockFileExists func(lockFilePath string) (bool, error),
	useRetryWatch func(
		ctx context.Context,
		watchFunc func(metav1.ListOptions) (watch.Interface, error),
		conditions func(watch.Event) (bool, error),
	) error,
) error {
	if val, err := checkLockFileExists(lockFilePath); err != nil {
		return fmt.Errorf("error checking lock file %q: %q", lockFilePath, err)
	} else if !val {
		glog.Infof("Lock file %q does not exist\n", lockFilePath)
		if err := useRetryWatch(ctx, watchFunc, WaitForDeviceUnregistered); err != nil {
			return fmt.Errorf("failed to use retry watch: %v", err)
		}
	}

	glog.Infof("Attempting to acquire lock on %q...\n", lockFilePath)
	if err := Acquire(lockFilePath); err != nil {
		return fmt.Errorf("error acquiring lock: %v", err)
	}
	return nil
}
