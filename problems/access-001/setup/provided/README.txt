ConfigMap report-config and Deployment reporter already exist in ${namespace}. Deployment reporter runs as the default ServiceAccount of the namespace.
Create the ServiceAccount report-reader with namespaced RBAC only (a Role and a RoleBinding in ${namespace}) and run reporter as report-reader with its token mounted.
Allowed: get, list and watch Pods; get ConfigMap report-config. Not allowed: delete Pods, get or list Secrets, create Deployments, list Pods in all namespaces or in kube-system, get any other ConfigMap.
Cluster-scoped objects that you create (ClusterRole, ClusterRoleBinding) are not removed when the problem is reset.
