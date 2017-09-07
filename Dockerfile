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

FROM golang:1.8.3-stretch as builder
WORKDIR /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators
COPY . .
RUN go build cmd/nvidia_gpu/nvidia_gpu.go

FROM gcr.io/google_containers/cos-nvidia-driver-install@sha256:456f272257d4557518afcaa7b0a1bba22d6861b20061ed5a1bd3f38aa7f8fb1a
COPY --from=builder /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/nvidia_gpu /usr/bin/device_plugins
RUN chmod a+x /usr/bin/device_plugins
CMD ["/usr/bin/device_plugins"]
