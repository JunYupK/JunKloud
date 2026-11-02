ResourceQuota team-quota and Deployment worker (2 replicas, container worker) already exist in ${namespace}. The Pods of worker are rejected by the quota, so the Deployment never becomes Ready.
Do not delete or change team-quota. Set the resources of container worker on the existing Deployment and create the LimitRange defaults.
Use image docker.io/library/busybox:1.37.0 and imagePullPolicy: Never; do not change the image or command.
