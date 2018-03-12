# drone-plugin-k8s-client

The plugin connects to an existing Kubernetes cluster and executes tasks wrapped into a K8s job.
It executes the logic built into a Docker image remotely (in the K8s cluster) and watches for the logs produced by the underlying pod.

The plugin primarily serves as a building block for the Banzai Cloud Pipeline CI/CD flow.

Check the ```.env``` template for configuration (the set of env variables "understood" by the plugin):


```sh
# the source repository (eg. git repository name)
export DRONE_REPO_NAME=repository

# workspace folder
export DRONE_WORKSPACE=/tmp

# build number
export DRONE_BUILD_NUMBER=0

# the image to be executed in the k8s cluster
export PLUGIN_ORIGINAL_IMAGE=bash

# the command to be executed in the original image
export PLUGIN_ORIGINAL_COMMANDS="echo 'hello Kubernauts!'"

# the k8s service account the job runs as
export PLUGIN_SERVICE_ACCOUNT=default

export PLUGIN_JOB_LABEL_SELECTOR=label-1
```

Issue the ```make list``` for the available operations.

