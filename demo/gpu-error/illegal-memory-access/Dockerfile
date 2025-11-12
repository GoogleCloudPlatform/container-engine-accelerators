# Copyright 2024 Google Inc. All rights reserved.
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

# Use a recent CUDA development image (includes nvcc compiler)
# --- Stage 1: Build ---
FROM nvidia/cuda:13.0.2-devel-ubuntu22.04 AS builder

WORKDIR /app
COPY vectorAdd.cu .

# Compile the CUDA code
RUN nvcc vectorAdd.cu -o vectorAdd

# --- Stage 2: Runtime ---
# Use a smaller base image containing only the CUDA runtime and nvidia-smi
FROM nvidia/cuda:13.0.2-base-ubuntu22.04

WORKDIR /app
# Copy only the compiled executable from the build stage
COPY --from=builder /app/vectorAdd .

# Command to run nvidia-smi for diagnostics and then the compiled program
CMD ["sh", "-c", "while true; do nvidia-smi && ./vectorAdd; echo '--- Waiting 30 seconds ---'; sleep 30; done"]