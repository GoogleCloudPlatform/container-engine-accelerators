## Overview

This document gives instructions on how to enable topology in GKE clusters on
A3M & A3U machines clusters.

The general outline for this to be successful is:
- We add labels for topology to nodes in the cluster with a daemonset
- We handle pod scheduling with a scheduling daemon
- Pods with the added scheduleGate are picked up and scheduled

## Prerequisites

For topology awareness to be enabled in A3M, a GKE node pool has to be created with
compact placement. Specifically, the `physical_host` attribute
[ref](https://cloud.google.com/compute/docs/instances/use-compact-placement-policies#verify-vm-location)
should be present for each GPU node in the cluster.

## Configuration

To initialize Kubernetes authentication for scripts:

```gcloud container clusters get-credentials [cluster name] --zone [cluster zone] --project [project id]```

## Usage

First copy this folder locally

Next create config maps for scripts required by pods

-   Run `kubectl create configmap topology-scheduler-scripts --namespace
    kube-system --from-file=schedule-daemon.py=schedule-daemon.py
    --from-file=label-nodes-daemon.py=label-nodes-daemon.py`

Next apply the service account config to the cluster:

-   Apply `service-account.yaml` config to the cluster by running `kubectl apply
    -f service-account.yaml`.

Now apply the scheduling and label daemons to the cluster so that pods will
automatically be scheduled with the correct schedulingGates

-   Apply `schedule-daemon.yaml` daemonset to the cluster by running `kubectl
    apply -f schedule-daemon.yaml`.
-   If GKE <1.31, apply `label-nodes-daemon.yaml` daemonset
    to the cluster by running `kubectl apply -f label-nodes-daemon.yaml`.

To let the daemon "pick up" the workload for scheduling, simply add a
schedulingGate that starts with ”gke.io/topology-aware-auto-”, for example:

```
  schedulingGates:
  - name: "gke.io/topology-aware-auto-my-job-name"
```
