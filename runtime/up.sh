#!/usr/bin/env bash
# Host bootstrap only. All learning commands run inside the Linux workspace.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
bash runtime/ensure-kind-network.sh
docker compose up -d --build "${1:-workspace}"
