# Copyright 2017 Google Inc. All rights reserved.
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
# limitations under the License.

TAG?=ubuntu1804
REGISTRY?=gcr.io/spectro-images-public/gpu/nvidia
IMAGE=ubuntu-nvidia-driver-installer
# works only for one worker node
WNODE=$(shell kubectl get nodes -A | grep -i none | cut -f 1 -d ' ')

container:
	docker build -t ${REGISTRY}/${IMAGE}:${TAG} .

push:
	docker push ${REGISTRY}/${IMAGE}:${TAG}

.PHONY: container push


buildnpush: container push


installer: buildnpush
	kubectl label nodes ${WNODE} spectrocloud.com/gpu-accelerator=""
	kubectl create -f spectro-nvidia-installer.yaml
	kubectl create -f image-pull.yaml
	kubectl create -f image-pull-default.yaml
	kubectl create -f spectro-cuda-add.yaml

uninstall:
	kubectl delete -f spectro-nvidia-installer.yaml
	kubectl delete -f image-pull.yaml
	kubectl delete -f spectro-cuda-add.yaml
	kubectl delete -f plugin.yaml