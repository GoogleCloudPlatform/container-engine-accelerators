# Copyright 2023 Google Inc. All rights reserved.
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

FROM golang:1.20-bullseye as builder
WORKDIR /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators
COPY . .
RUN go build -o device_injector nri_device_injector/nri_device_injector.go
RUN chmod a+x /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/device_injector

FROM us.gcr.io/gke-release/gke-distroless/bash:gke_distroless_20250207.00_p0@sha256:dce99ff7978706ab3cabfeaae65d404d033d27a020e2c32a2f3a1daffd033343
COPY --from=builder /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/device_injector /usr/bin/device_injector
CMD ["/usr/bin/device_injector"]