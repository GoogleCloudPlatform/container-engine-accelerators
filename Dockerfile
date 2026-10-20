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

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators
COPY . .
RUN if [ "${TARGETARCH}" = "arm64" ] && [ "${BUILDARCH}" != "arm64" ]; then \
    apt update && \
    apt install -yq --no-install-recommends \
        gcc-aarch64-linux-gnu libc6-dev-arm64-cross; \
        CC=aarch64-linux-gnu-gcc; \
    fi && \
    GOTOOLCHAIN=local GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=1 CC=${CC} \
      go build cmd/nvidia_gpu/nvidia_gpu.go
# GPU bug-report executor: pure Go and statically linked, so it needs no cross-compiler.
RUN GOTOOLCHAIN=local GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 \
      go build -trimpath -ldflags="-s -w" -o gpu_bug_report ./cmd/gpu_bug_report
RUN chmod a+x /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/nvidia_gpu \
              /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/gpu_bug_report

FROM gcr.io/distroless/base:latest
COPY --from=builder /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/nvidia_gpu /usr/bin/nvidia-gpu-device-plugin
COPY --from=builder /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/gpu_bug_report /usr/bin/gpu-bug-report
CMD ["/usr/bin/nvidia-gpu-device-plugin", "-logtostderr"]
# Use the CMD below to make the device plugin expose prometheus endpoint with container level GPU metrics
#CMD ["/usr/bin/nvidia-gpu-device-plugin", "-logtostderr", "-v=10", "--enable-container-gpu-metrics"]
