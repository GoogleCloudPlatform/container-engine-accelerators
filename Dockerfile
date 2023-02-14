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

FROM golang:1.19 as builder
WORKDIR /go/src/github.com/melody789/container-engine-accelerators
COPY . .
RUN go build cmd/nvidia_gpu/nvidia_gpu.go
RUN chmod a+x /go/src/github.com/melody789/container-engine-accelerators/nvidia_gpu

FROM gcr.io/distroless/base:latest
COPY --from=builder /go/src/github.com/melody789/container-engine-accelerators/nvidia_gpu /usr/bin/nvidia-gpu-device-plugin
CMD ["/usr/bin/nvidia-gpu-device-plugin", "-logtostderr"]
# Use the CMD below to make the device plugin expose prometheus endpoint with container level GPU metrics
#CMD ["/usr/bin/nvidia-gpu-device-plugin", "-logtostderr", "-v=10", "--enable-container-gpu-metrics"]
