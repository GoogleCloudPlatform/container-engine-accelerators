.PHONY: all push build

TAG?=ubuntu
REGISTRY?=gcr.io/spectro-images-public/gpu/nvidia
IMAGE=ubuntu-nvidia-add

build:
	docker build -t ${REGISTRY}/${IMAGE}:${TAG} .

push:
	docker push ${REGISTRY}/${IMAGE}:${TAG}

all: build

buildnpush: build push