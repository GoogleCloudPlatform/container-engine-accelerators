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

FROM tensorflow/tensorflow:1.9.0

RUN apt-get update && apt-get install -y --no-install-recommends \
    git libsm6 libxext6 libxrender1

RUN git clone https://github.com/vishh/tf-serving-k8s-tutorial.git /serving-client && pip install tensorflow-serving-api opencv-python opencv-contrib-python grpcio requests

WORKDIR /serving-client/client
ENTRYPOINT ["python"]