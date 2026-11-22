## Overview

This document gives instructions on how to enable predictor in GKE clusters.

The general outline for this to be successful is:
- We add labels for predictor to nodes in the cluster with a daemonset

## Prerequisites

For predictor awareness to be enabled, `compute.googleapis.com/instance/gpu/failure_prediction_status`
[ref](https://monitoring.corp.google.com/explorer?duration=3600&utc_end=0&refresh=90&legend=bottom&mash=Fetch(Precomputed(%27cloud-cluster-vm%27,%20%27compute_module%27,%20%27compute.googleapis.com%2Finstance%2Fgpu%2Ffailure_prediction_status%27))%0A%7C%20Window(Align(%2710m%27))&q_namespaces=cloud_prod)
should be present in CloudMonarch for each GPU node in the cluster.

## Configuration

To initialize Kubernetes authentication for scripts:

```gcloud container clusters get-credentials [cluster name] --zone [cluster zone] --project [project id]```

Grant KSU with Cloud Monarch viewer and Computer viewer permissions:

```
gcloud projects add-iam-policy-binding projects/[project_name] \
    --role="roles/monitoring.viewer" \
    --member=principal://iam.googleapis.com/projects/[project_id]/locations/global/workloadIdentityPools/[project_name].svc.id.goog/subject/ns/kube-system/sa/predictor-scheduler \
    --condition=None


gcloud projects add-iam-policy-binding projects/[project_name] \
    --role="roles/compute.viewer" \
    --member=principal://iam.googleapis.com/projects/[project_id]/locations/global/workloadIdentityPools/[project_name].svc.id.goog/subject/ns/kube-system/sa/predictor-scheduler \
    --condition=None
```

## Usage

First copy this folder locally

Next create config maps for scripts required by pods

-   Run `kubectl create configmap predictor-scheduler-scripts --namespace kube-system 
        --from-file=label-nodes.py=label-nodes.py`

Next apply the service account config to the cluster:

-   Apply `service-account.yaml` config to the cluster by running `kubectl apply
    -f service-account.yaml`.

Now apply `label-nodes-cronjob.yaml` to the cluster. This will create the CronJob,
which is scheduled to run every 10 minutes.

-   Apply `label-nodes-cronjob.yaml` to the cluster by running
    `kubectl apply -f label-nodes-cronjob.yaml`.

You can check the status of the CronJob and its runs using:

```
# Check the CronJob definition
kubectl get cronjob label-nodes-cronjob -n kube-system

# Check the jobs created by the CronJob
kubectl get jobs -n kube-system | grep label-nodes-cronjob

# Check the pods created by the most recent job
kubectl get pods -n kube-system | grep label-nodes-cronjob

# View logs from a specific pod (replace pod name)
kubectl logs -n kube-system <pod_name>
```

## Verification

You can also check the labels on your GPU nodes:

```
kubectl get nodes -l nvidia.com/gpu -o custom-columns=NAME:.metadata.name,LABEL:.metadata.labels."gke\.io/recommended-to-run-large-training-workload"
```
