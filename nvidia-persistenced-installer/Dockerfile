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

FROM --platform=$BUILDPLATFORM golang:1.24-bullseye AS builder

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
      go build nvidia-persistenced-installer/nvidia_persistenced_installer.go
RUN chmod a+x /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/nvidia_persistenced_installer

# Final image requires ldconfig binary so we will copy it from the debian distribution.
FROM marketplace.gcr.io/google/debian12 AS sbin_builder

FROM gcr.io/distroless/base:latest
COPY --from=sbin_builder /usr/sbin/ldconfig /usr/sbin
COPY --from=builder /go/src/github.com/GoogleCloudPlatform/container-engine-accelerators/nvidia_persistenced_installer /usr/bin/nvidia-persistenced-installer
CMD ["/usr/bin/nvidia-persistenced-installer", "-logtostderr"]
