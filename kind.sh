#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

CREATE_KIND_CLUSTER=1
KIND_CLUSTER_NAME="validator-elector"
KIND_IMAGE="kindest/node:v1.16.15"
export KUBECONFIG="${PWD}/e2e/.kubeconfig"

if ! [ -z "$CREATE_KIND_CLUSTER" ]; then
    kind create cluster \
         --verbosity 4 \
         --config e2e/kind.yaml \
         --image ${KIND_IMAGE} \
         --name ${KIND_CLUSTER_NAME}
fi
    
docker build -t validator-elector:test .
kind load docker-image \
     --name="${KIND_CLUSTER_NAME}" \
     validator-elector:test
