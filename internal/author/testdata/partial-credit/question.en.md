# Scale the web Deployment

Weight: 10 points. Namespace: `${namespace}`.

A ConfigMap `settings` already exists in namespace `${namespace}`. Create a
Deployment named `web` there, running the image `busybox:1.37.0` with the
command `sleep 3600`, and make it run `${replicas}` replicas.
