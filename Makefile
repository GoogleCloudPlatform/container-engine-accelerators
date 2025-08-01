# Copyright 2015 Google Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and

GO := go
pkgs  = $(shell $(GO) list ./... | grep -v -e vendor -e nri_device_injector)

all: presubmit

test:
	@echo ">> running tests"
	@$(GO) test -short -race $(pkgs)

format:
	@echo ">> formatting code"
	@$(GO) fmt $(pkgs)

vet:
	@echo ">> vetting code"
	@$(GO) vet $(pkgs)

presubmit: vet
	@echo ">> checking go formatting"
	@./build/check_gofmt.sh .
	@echo ">> checking file boilerplate"
	@./build/check_boilerplate.sh

TAG=$(shell cat VERSION)
REGISTRY?=gcr.io/google-containers
IMAGE=nvidia-gpu-device-plugin
PARTITION_GPU_IMAGE=nvidia-partition-gpu
FASTSOCKET_INSTALLER_IMAGE=fastsocket-installer
DEVICE_INJECTOR_IMAGE=nri-device-injector
NVIDIA_PERSISTENCED_IMAGE=nvidia-persistenced-installer
ALL_ARCHITECTURES=amd64 arm64

build:
	cd cmd/nvidia_gpu; go build nvidia_gpu.go

container:
	docker buildx build --pull --load -t ${REGISTRY}/${IMAGE}:${TAG} .

container-multi-arch:
	@for arch in $(ALL_ARCHITECTURES); do \
	  docker buildx build --pull --load --platform linux/$${arch} -t ${REGISTRY}/${IMAGE}-$${arch}:${TAG} . ; \
	done

push:
	gcloud docker -- push ${REGISTRY}/${IMAGE}:${TAG}

push-all:
	@for arch in $(ALL_ARCHITECTURES); do docker push ${REGISTRY}/${IMAGE}-$${arch}:${TAG}; done

push-multi-arch:
	docker manifest create --amend ${REGISTRY}/${IMAGE}:${TAG} $(shell echo $(ALL_ARCHITECTURES) | sed -e "s~[^ ]*~${REGISTRY}/${IMAGE}\-&:${TAG}~g")
	@for arch in $(ALL_ARCHITECTURES); do docker manifest annotate --os linux --arch $${arch} ${REGISTRY}/${IMAGE}:${TAG} ${REGISTRY}/${IMAGE}-$${arch}:${TAG}; done
	docker manifest push --purge ${REGISTRY}/${IMAGE}:${TAG}

partition-gpu:
	docker buildx build --pull --load -t ${REGISTRY}/${PARTITION_GPU_IMAGE}:${TAG} -f partition_gpu/Dockerfile .

partition-gpu-multi-arch:
	@for arch in $(ALL_ARCHITECTURES); do \
	  docker buildx build --pull --load --platform linux/$${arch} -t ${REGISTRY}/${PARTITION_GPU_IMAGE}-$${arch}:${TAG} -f partition_gpu/Dockerfile . ; \
	done

fastsocket_installer:
	docker build --pull -t ${REGISTRY}/${FASTSOCKET_INSTALLER_IMAGE}:${TAG} -f fast-socket-installer/image/Dockerfile .

nri-device-injector:
	docker build --pull -t ${REGISTRY}/${DEVICE_INJECTOR_IMAGE}:${TAG} -f nri_device_injector/Dockerfile .

nvidia_persistenced_installer:
	docker buildx build --pull --load -t ${REGISTRY}/${NVIDIA_PERSISTENCED_IMAGE}:${TAG} -f nvidia-persistenced-installer/Dockerfile .

nvidia-persistenced-installer-multi-arch:
	@for arch in $(ALL_ARCHITECTURES); do \
	  docker buildx build --pull --load --platform linux/$${arch} -t ${REGISTRY}/${NVIDIA_PERSISTENCED_IMAGE}-$${arch}:${TAG} -f partition_gpu/Dockerfile . ; \
	done

.PHONY: all format test vet presubmit build container push partition-gpu

bin/device-injector-test:
	$(GO) test -c ./nri_device_injector -o ./bin/device-injector-test 

.PHONY: device-injector-test
device-injector-test: bin/device-injector-test
	sudo ./bin/device-injector-test -test.v 

.PHONY: clean
clean:
	@rm -rf bin
