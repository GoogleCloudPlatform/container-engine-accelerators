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


EXPERIMENT_ID='resnet-1'
BATCH_SIZES=(4 8 16 32 64 128)
TRAIN_STEPS=(10000 20000 30000 40000)

rm -rf $EXPERIMENT_ID
mkdir $EXPERIMENT_ID


for BATCH_SIZE in "${BATCH_SIZES[@]}"
do
  for TRAIN_STEP in "${TRAIN_STEPS[@]}"
  do

  JOB_ID=${EXPERIMENT_ID}-$BATCH_SIZE-$TRAIN_STEP

  cat >$EXPERIMENT_ID/$EXPERIMENT_ID-$BATCH_SIZE-$TRAIN_STEP.yaml <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB_ID}
  labels:
    experiment-id: '${EXPERIMENT_ID}'
    batch-size: '${BATCH_SIZE}'
    train-steps: '${TRAIN_STEP}'
spec:
  template:
    metadata:
      labels:
        experiment-id: '${EXPERIMENT_ID}'
        batch-size: '${BATCH_SIZE}'
        train-steps: '${TRAIN_STEP}'
EOF
cat >>$EXPERIMENT_ID/$EXPERIMENT_ID-$BATCH_SIZE-$TRAIN_STEP.yaml <<'EOF'
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
          - --data_dir=gs://gke-k8s-gcp-next-demo/imagenet
          - --model_dir=gs://gke-k8s-gcp-next-demo/models/$(EXPERIMENT_ID)/$(BATCH_SIZE)/
          - --train_batch_size=$(BATCH_SIZE)
          - --train_steps=$(TRAIN_STEPS)
        env:
        - name: EXPERIMENT_ID
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['experiment-id']
        - name: BATCH_SIZE
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['batch-size']
        - name: TRAIN_STEPS
          valueFrom:
            fieldRef:
              fieldPath: metadata.labels['train-steps']
        resources:
          limits:
            nvidia.com/gpu: 1

EOF

  done
done