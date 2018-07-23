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

#!/bin/bash

EXPERIMENT_ID="resnet-$(date "+%y-%m-%d-%H-%M-%S")"

BASE_LEARNING_RATES=(0.001 0.01 0.1 0.05)
BATCH_SIZES=(16 32)
DEPTH_CHOICES=(34 50 101 152)

EPOCHS=90
NUM_IMAGES=1281167

echo "Experiment number ${EXPERIMENT_ID}"
rm -rf $EXPERIMENT_ID
mkdir $EXPERIMENT_ID

for DEPTH in "${DEPTH_CHOICES[@]}"
  do
    for BATCH_SIZE in "${BATCH_SIZES[@]}"
      do
        for BASE_LEARNING_RATE in "${BASE_LEARNING_RATES[@]}"
          do
          JOB_ID=${EXPERIMENT_ID}-$BATCH_SIZE-$DEPTH-$BASE_LEARNING_RATE
          TRAIN_STEPS=$((EPOCHS*NUM_IMAGES/BATCH_SIZE))
          cat >$EXPERIMENT_ID/$EXPERIMENT_ID-$BATCH_SIZE-$DEPTH-$BASE_LEARNING_RATE.yaml <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB_ID}
  labels:
    experiment-id: ${EXPERIMENT_ID}
spec:
  template:
    metadata:
      labels:
        experiment-id: ${EXPERIMENT_ID}
    spec:
      restartPolicy: Never
      containers:
      - name: resnet-gpu
        image: gcr.io/vishnuk-cloud/tf-models-gpu:1.0
        command:
          - python
          - /tensorflow_models/models/official/resnet/resnet_main.py
          - --use_tpu=False
          - --tpu=
          - --precision=float32
          - --data_dir=gs://cloudtpu-imagenet-data/train
          - --model_dir=gs://vishh/tensorflow/resnet-gpu-train/${EXPERIMENT_ID}/${BATCH_SIZE}-${BASE_LEARNING_RATE}-${DEPTH}
          - --train_batch_size=${BATCH_SIZE}
          - --train_steps=${TRAIN_STEPS}
          - --resnet_depth=${DEPTH}
          - --base_learning_rate=${BASE_LEARNING_RATE}
          - --steps_per_eval=25000
        env:
        - name: EXPERIMENT_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['experiment-id']
        resources:
          limits:
            nvidia.com/gpu: 8

EOF

        done
    done
done
