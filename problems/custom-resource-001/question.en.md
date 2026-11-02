Deployment `ledger` already runs in `${namespace}`. The platform administrator installed a custom resource type for backups in this cluster. Work with namespaced objects in `${namespace}` only: a cluster-scoped object that you create is not removed when the problem is reset.

Find the custom resource type that the platform provides for backups and write its full resource name (`<plural>.<group>`), on one line, to `answers/backup-crd.txt` in your workspace.

Then create a `Backup` named `nightly` in `${namespace}` that runs at `0 2 * * *` (schedule), backs up the Deployment `ledger` (target kind `Deployment`, target name `ledger`) and keeps backups for 7 retention days. Create it as an object of that custom resource type: a ConfigMap or any other kind of object does not count, even if it holds the same values. The type's schema tells you which fields it requires.

Each of the following earns 1 point: `answers/backup-crd.txt` contains the full resource name of the backup custom resource type (surrounding blank space is ignored); a `Backup` named `nightly` exists in `${namespace}`; its schedule is `0 2 * * *`; its target is the `Deployment` named `ledger`; and its retention is 7 days. The last three points are only awarded when the `Backup` exists. The order of the fields in your manifest does not matter, and optional fields of the schema are allowed.
