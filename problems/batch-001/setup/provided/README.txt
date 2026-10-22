The namespace starts with a suspended parcel-once Job and a suspended parcel-clock CronJob. Inspect and repair them.

Provided multi-architecture base: docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
Use image docker.io/library/busybox:1.37.0 and imagePullPolicy: Never in Kubernetes workloads. Runtime imports this exact version tag from the pinned digest above for the native node architecture; the environment certificate records that digest. Suggested per-container requests: 10m CPU/8Mi; limits: 75m/32Mi.
