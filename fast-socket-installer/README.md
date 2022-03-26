# NCCL Fast Socket Installer
NCCL Fast Socket is a transport layer plugin to improve NCCL collective
communication performance on Google Cloud.


## To deploy NCCL fast socket installer on all fast-socket labeled nodes in GKE cluster
- ### Add fast-socket node label 
  After creating the GKE cluster, add
 `cloud.google.com/gke-nccl-fastsocket: "true"` on the node YAML file
- ### Deploy NCCL fast socket installer
  Run `kubectl apply -f fast-socket-installer.yaml`