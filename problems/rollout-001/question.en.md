Repair the existing apps/v1 Deployment `dispatch` in `${namespace}`. The provided notes identify `stable-v1` as the known stable application version; the current `broken-v2` template fails readiness.

Use container `web` with the pinned provided image `docker.io/library/busybox:1.37.0` and environment variable `RELEASE=stable-v1`. Set exactly 2 replicas and strategy `RollingUpdate` with numeric `maxSurge: 1` and `maxUnavailable: 0`. Make the application run successfully and print a complete `version=stable-v1` log line from every Deployment Pod.

The controller must observe the current generation. All 2 desired replicas must be updated, Ready and Available, with no surplus old replicas. The Pods of the current controller revision must stay Ready and running without a restart for the existing 10-second stability window (60-second observation limit). A healthy older revision cannot satisfy this goal. There is no required command sequence, past rollback command, or numeric revision value.

Each group earns 1 point: Deployment existence; image and stable version; strategy and replica goal; current stable rollout; actual version logs. The broken initial Deployment earns only the existence point (1/5). Equivalent manifests and output commands are accepted.
