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

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o device_injector nri_device_injector/nri_device_injector.go
RUN chmod a+x /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/device_injector

FROM gke.gcr.io/gke-distroless/bash:gke_distroless_20260207.00_p0@sha256:002b4b70bf122aaed02d9ba7158a5ce8424a5cc4342cc304ab6c254943f6a5da
COPY --from=builder /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/device_injector /usr/bin/device_injector
CMD ["/usr/bin/device_injector"]
