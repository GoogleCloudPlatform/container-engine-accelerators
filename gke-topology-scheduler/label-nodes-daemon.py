#!/usr/bin/env python

# Copyright 2024 Google Inc. All Rights Reserved.
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
"""Daemon to update Kubernetes node labels based on GCE VM metadata."""

import time
from typing import Dict

from kubernetes import client
from kubernetes import config
import requests


def update_node_labels(kube: client.CoreV1Api) -> None:
  """Updates Kubernetes node labels based on GCE VM metadata."""
  node_name_url = "http://metadata.google.internal/computeMetadata/v1/instance/name"
  metadata_url = "http://metadata.google.internal/computeMetadata/v1/instance/attributes/physical_host"
  headers = {"Metadata-Flavor": "Google"}

  response = requests.get(node_name_url, headers=headers)

  if response.status_code == 200:
    node_name = response.text
  else:
    print("Node name not found")
    return

  response = requests.get(metadata_url, headers=headers)

  if response.status_code == 200:
    physical_host = response.text
  else:
    print("physical host not found")
    return

  cluster, rack, host = physical_host.split("/")[1:]

  node_labels: Dict[str, str] = {
      "topology.gke.io/cluster": cluster,
      "topology.gke.io/rack": rack,
      "topology.gke.io/host": host,
  }

  kube.patch_node(node_name, {"metadata": {"labels": node_labels}})  # type: ignore
  print(f"Updated labels on node {node_name}: {node_labels}")


if __name__ == "__main__":
  # Kubernetes configuration
  config.load_incluster_config()
  client = client.CoreV1Api()

  while True:
    print("Starting node update")
    # Update node labels
    update_node_labels(client)
    time.sleep(600)
