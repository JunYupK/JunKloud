Deployment ledger already exists in ${namespace}. The platform administrator installed a custom resource type for backups in the cluster.
Find it with kubectl, write its full resource name (<plural>.<group>) to answers/backup-crd.txt in this workspace, and create the Backup nightly in ${namespace}.
Work with namespaced objects in ${namespace} only: a cluster-scoped object that you create is not removed when the problem is reset.
