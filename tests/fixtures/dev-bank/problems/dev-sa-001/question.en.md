# Stop the builder ServiceAccount mounting its token

Weight: 5 points. Namespace: `dev-sec`.

The ServiceAccount `builder` in namespace `dev-sec` automatically mounts its
API token into pods. Change the existing ServiceAccount so that it does not.
