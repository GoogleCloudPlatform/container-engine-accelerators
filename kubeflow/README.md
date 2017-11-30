# Kubeflow Anywhere

This document detals the steps needed to run the kubeflow project in different environments, such as Minikube (local laptop). Google Kubernetes Engine, etc. 

## Quick Start

In order to quickly setup all components of the stack, run:

```commandline
kubectl apply -f manifests/ -R
```

The above command sets up JupyterHub, an API for training using Tensorflow, and a set of deployment files for serivng. 
Used together, these serve as configuration that can help a user go from training to serving using Tensorflow with minimal
effort in a portable fashion between different environments. You can refer Instructions for using each of these components below. 

## Minikube

[Minikube](https://github.com/kubernetes/minikube) is a tool that makes it easy to run Kubernetes locally. Minikube runs a single-
node Kubernetes cluster inside a VM on your laptop for users looking to try out Kubernetes or develop with it day-to-day. 
The below steps apply to a minikube cluster - the latest version as of writing this documetation is 0.23.0. You must also have 
kubectl configured to access minikube.

### Bringing up a Notebook

Once you create all the manifests needed for JupyterHub, a load balancer service is created. You can check its existence using the kubectl commandline.

```commandline
kubectl get svc

kubernetes   ClusterIP      10.43.240.1     <none>        443/TCP        16h
tf-hub-0     ClusterIP      None            <none>        8000/TCP       2s
tf-hub-lb    LoadBalancer   10.43.252.191   <pending>     80:31975/TCP   0s
```

If you see output similar to the above, you're ready to proceed to the next step. Now, you can find the URL on which the service is
being exposed.

```
minikube service tf-hub-lb --url

http://x.y.z.w:31942
``` 

Once you've found the URL, you can visit that in your browser, and get access to your Hub. The hub by default is configured to take any username/password combination. After entering the username and password, you can start a single-notebook server,
request any resources (memory/CPU/GPU), and then proceed to perform single node training.

###  Single node Training

TODO(vish)

### Distributed Training

TODO(jlewi)

### Serve Model

TODO(owensk)

## Google Kubernetes Engine

[Google Kubernetes Engine](https://cloud.google.com/kubernetes-engine/) is a managed environment for deploying 
Kubernetes applications powered by Google Cloud.

### Bringing up a Notebook

Once you create all the manifests needed for JupyterHub, a load balancer service is created. You can check its existence using the kubectl commandline.

```commandline
kubectl get svc

kubernetes   ClusterIP      10.43.240.1     <none>        443/TCP        16h
tf-hub-0     ClusterIP      None            <none>        8000/TCP       2s
tf-hub-lb    LoadBalancer   10.43.252.191   <pending>     80:31975/TCP   0s
```

In a minute or so, the LoadBalancer service should get an external IP address associated with it. Once you have an external IP, 
you can proceed to visit that in your browser. The hub by default is configured to take any username/password combination. After entering the username and password, you can start a single-notebook server,
request any resources (memory/CPU/GPU), and then proceed to perform single node training.

Note that the public IP address is exposed to the internet and is an unsecured endpoint. For a production deployment, 
refer to the [detailed documentation](jupyterhub/README.md) on how to set up SSL and authentication for your JupyterHub. 

### Single node Training

TODO(vish)

### Distributed Training

TODO(jlewi)

### Serve Model

TODO(owensk)

## Components

### JupyterHub

JupyterHub allows users to create, and manage multiple single-user Jupyter notebooks. Note that the configuration provided 
aims at simplicity. If you want to configure it for production scenarios, including SSL, authentication, etc, refer to the [detailed documentation](jupyterhub/README.md) on Jupyterhub.

### Tensorflow Training Operator

TODO(jlewi)

### Tensorflow Serving

TODO(owensk)

## The Kubeflow Mission

TODO(aronchick)

## Roadmap

TODO(aronchick)
