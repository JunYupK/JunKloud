# JunKloud doctor "image-build" probe. The doctor writes a per-run token to
# probe-token next to this file, builds the context with the Docker CLI
# against the shared engine, loads the image into the kind node and runs it
# as a pod, which must report that token. The base is the pinned probe image
# (runtime/versions.lock.yaml platform.probe_image), so the build fetches
# nothing else and runs no build step in a container.
FROM docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
COPY probe-token /probe-token
USER 65534:65534
CMD ["sh", "-c", "cat /probe-token > /dev/termination-log"]
