#!/usr/bin/env bash
# The shared kind bridge is connectivity, not a disposable Compose resource.
# Never replace, reconfigure or remove an existing network.
set -euo pipefail
if driver="$(docker network inspect --format '{{.Driver}}' kind 2>/dev/null)"; then
    if [ "${driver}" != bridge ]; then
        echo 'The existing kind network is not a bridge; inspect Docker networking before starting.' >&2
        exit 1
    fi
else
    docker network create --driver bridge --label io.x-k8s.kind.role=network kind >/dev/null
fi
