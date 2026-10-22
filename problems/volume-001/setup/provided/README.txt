ledger-data and spare-data PVCs plus ledger Deployment are provided. Both claims bind through bootstrap mounts. Wait for Ready/Bound; repair the intentionally EmptyDir /data mount to ledger-data. Auxiliary bootstrap mounts need not be preserved after initial binding.

Provided multi-architecture base: docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
Use image docker.io/library/busybox:1.37.0 and imagePullPolicy: Never in Kubernetes workloads. Runtime imports this exact version tag from the pinned digest above for the native node architecture; the environment certificate records that digest. Suggested per-container requests: 10m CPU/8Mi; limits: 75m/32Mi.
