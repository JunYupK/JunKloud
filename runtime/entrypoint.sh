#!/usr/bin/env bash
# Container entrypoint for the workspace/dev services.
#
# Must never overwrite a .bashrc (or .bash_aliases) the learner already has:
# the home directory lives on a named volume that outlives container
# recreation, and a user's aliases/files in it are independent of problem
# resets. This only ever fills in the file if it is completely absent, and
# then simply hands off to the container's command.
set -euo pipefail

seed_if_missing() {
    local target="$1"
    local skel="$2"
    if [ ! -e "${target}" ] && [ -e "${skel}" ]; then
        cp "${skel}" "${target}"
    fi
}

seed_if_missing "${HOME}/.bashrc" /etc/skel/.bashrc
seed_if_missing "${HOME}/.bash_aliases" /etc/skel/.bash_aliases

exec "$@"
