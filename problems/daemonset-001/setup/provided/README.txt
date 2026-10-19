No workload is initially installed. The cluster has one Linux kind node; allow its control-plane taint if present.

Provided multi-architecture base: docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
Use imagePullPolicy: Never in Kubernetes workloads; this exact image is preloaded. Suggested per-container requests: 10m CPU/8Mi; limits: 75m/32Mi.
