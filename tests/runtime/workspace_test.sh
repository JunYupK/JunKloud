#!/usr/bin/env bash
# Runtime test for the reproducible Linux dev/workspace environment.
#
# This script drives `docker compose` against runtime/compose.yaml using an
# isolated, throwaway project name so it never touches a real learner's
# persistent `learner-home` volume -- every volume this run creates is
# removed again in the cleanup trap below ("temporary volumes only").
#
# It verifies:
#   1. the workspace container is Linux, running on the host's native CPU
#      architecture (no silent QEMU emulation),
#   2. a file and a shell alias a user adds inside the container survive the
#      container being recreated (the home volume, not the container layer,
#      holds state), and
#   3. kind/kubectl/Helm inside the image report exactly the versions pinned
#      in versions.lock.yaml.
#
# Runs from inside the Linux dev/workspace container (Docker socket shared
# with the host) or directly on a host that has docker + the compose plugin.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PROJECT="ckad-runtime-test-$$"

# `docker compose` resolves a bare `compose.yaml` (and the build context "."
# and volume source "..") relative to the current directory, so cd into
# runtime/ once up front instead of passing an absolute -f path.
cd "${REPO_ROOT}/runtime"

# On Git-Bash/MSYS hosts (Windows), bash rewrites POSIX-looking absolute
# paths in command-line *arguments* into Windows paths -- including ones
# meant for the *container's* filesystem, like /home/learner/... passed to
# `cat` through `docker compose exec`. Disabling that rewrite is a no-op on
# real Linux, so it is safe to always set. (The `cd` above means no host-side
# path argument needs conversion anymore, so this is safe to set globally.)
export MSYS_NO_PATHCONV=1

compose() {
  docker compose -p "${PROJECT}" "$@"
}

cleanup() {
  compose down -v --rmi local --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# Reads the pinned version for `tools.<name>.version` straight out of
# versions.lock.yaml on the machine running *this script* (not inside the
# throwaway container compose() talks to). That file is always reachable
# here via REPO_ROOT, computed from this script's own path, regardless of
# whether this script runs on the host or inside the dev/workspace container.
#
# This deliberately avoids reading the file through a bind mount inside the
# nested test container: when this script itself runs inside a container
# (Docker-outside-of-Docker, sharing the host's Docker socket), a relative
# bind-mount source like "..:/src" is resolved by the *daemon*, not by the
# container issuing the request -- so a second, nested container cannot
# reliably see the first container's bind-mounted repo. A tiny awk scalar
# reader for our own fixed-shape YAML sidesteps that entirely.
lock_version() {
  local tool="$1"
  awk -v tool="${tool}:" '
    $0 ~ ("^  " tool "$") { found=1; next }
    found && /^  [A-Za-z_]+:/ { exit }
    found && /^    version:/ {
      line = $0
      sub(/^[^"]*"/, "", line)
      sub(/".*$/, "", line)
      print line
      exit
    }
  ' "${REPO_ROOT}/versions.lock.yaml"
}

echo "==> building workspace image (project ${PROJECT})"
compose build workspace

echo "==> starting workspace container"
compose up -d workspace

echo "==> checking OS and native CPU architecture"
os="$(compose exec -T workspace uname -s)"
test "${os}" = "Linux" || fail "expected Linux inside the container, got '${os}'"

container_arch="$(compose exec -T workspace uname -m)"
host_arch="$(docker version --format '{{.Server.Arch}}')"
case "${host_arch}" in
  amd64) want_arch="x86_64" ;;
  arm64) want_arch="aarch64" ;;
  *) fail "unrecognised host Docker server arch '${host_arch}'" ;;
esac
test "${container_arch}" = "${want_arch}" \
  || fail "container reports arch '${container_arch}', expected native '${want_arch}' (host engine arch: ${host_arch}); emulation suspected"

echo "==> writing user state (file + alias) into the home volume"
compose exec -T workspace bash -c 'echo keep > /home/learner/persist-check'
compose exec -T workspace bash -c 'echo "alias user_check=keep" >> /home/learner/.bashrc'

echo "==> recreating the container to prove the volume (not the container layer) persists state"
compose rm -sf workspace
compose up -d workspace

echo "==> verifying the file and alias survived reconnect"
persisted="$(compose exec -T workspace cat /home/learner/persist-check)"
test "${persisted}" = "keep" || fail "expected persisted file content 'keep', got '${persisted}'"

compose exec -T workspace bash -ic 'alias user_check' | grep -q keep \
  || fail "alias 'user_check' did not survive container recreation"

echo "==> verifying kind/kubectl/Helm versions match versions.lock.yaml"
kind_want="$(lock_version kind | tr -d '\r')"
kind_have="$(compose exec -T workspace kind version | awk '{print $2}' | tr -d '\r')"
test "${kind_have}" = "${kind_want}" || fail "kind version '${kind_have}' != lock '${kind_want}'"

kubectl_want="$(lock_version kubectl | tr -d '\r')"
kubectl_have="$(compose exec -T workspace sh -c 'kubectl version --client -o yaml | yq ".clientVersion.gitVersion"' | tr -d '\r')"
test "${kubectl_have}" = "${kubectl_want}" || fail "kubectl version '${kubectl_have}' != lock '${kubectl_want}'"

helm_want="$(lock_version helm | tr -d '\r')"
helm_have="$(compose exec -T workspace sh -c "helm version --short | cut -d'+' -f1" | tr -d '\r')"
test "${helm_have}" = "${helm_want}" || fail "helm version '${helm_have}' != lock '${helm_want}'"

echo "==> verifying the Docker socket was passed through from the host"
compose exec -T workspace docker info >/dev/null \
  || fail "docker CLI inside the workspace container cannot reach the host engine over the shared socket"

echo "ALL CHECKS PASSED"
