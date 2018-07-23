# Copyright 2018 Google Inc. All rights reserved.
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

FROM nvidia/cuda:9.0-cudnn7-runtime-ubuntu16.04

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    python-pip \
    python-dev \
    git \
    libgomp1 \
    && \
    rm -rf /var/lib/apt/lists/*

RUN pip install setuptools
RUN pip install tensorflow-gpu==1.8.0

# Checkout TensorFlow 1.9 TPU models.
RUN git clone -b r1.8 https://github.com/tensorflow/tpu.git /tensorflow_models/
