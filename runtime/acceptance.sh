#!/usr/bin/env bash
# Release-acceptance runbook for a real device (Mac or Windows with Docker
# Desktop). Run it INSIDE the dev container:
#
#   bash runtime/up.sh dev            # Windows: runtime/up.ps1 -Service dev
#   docker compose exec dev bash
#   bash runtime/acceptance.sh        # add --cold on a Docker with no pulled images
#
# What it does, in order:
#   1. prints the host facts (architecture, Docker memory/CPU/version);
#   2. runs `ckad doctor` and `ckad bank validate` for every problem, timing
#      each one, in a THROWAWAY state directory so your own practice state,
#      kubeconfig and solutions are never touched;
#   3. removes the throwaway environment (matched by its exact environment ID);
#   4. runs `make test-acceptance` (prepare, model solution, grade, reset,
#      three times in a row) with the evidence kept in $HOME/junkloud-acceptance;
#   5. prints a summary table to paste back.
#
# If `ckad doctor` reports a capability FAIL (for example network-policy on a
# host whose CNI does not enforce NetworkPolicy), the capability names are
# exported in JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES. The acceptance test then
# runs a labelled PARTIAL exam, and the summary says so. It is never reported
# as mock-01.
#
# This script never reports a result it did not observe: a phase that did not
# run is printed as NOT RUN with the reason.

set -uo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# problem_ids prints the problem IDs of the bank, one per line.
problem_ids() {
    local bank="${CKAD_BANK_DIR:-/src}"
    local d
    for d in "${bank}"/problems/*/; do
        [ -d "${d}" ] && basename "${d}"
    done
}

# unsupported_from_doctor <doctor-output-file> prints the comma-separated
# capability names whose check is not PASS (empty when all PASS). It fails when
# the output holds no capability line at all, so a broken doctor can never be
# read as "everything is supported".
unsupported_from_doctor() {
    local file="$1" caps="dns service network-policy ingress storage metrics image-build"
    local seen=0 out="" cap line status
    for cap in ${caps}; do
        line="$(grep -E "^${cap}: " "${file}" | head -n 1 || true)"
        [ -z "${line}" ] && continue
        seen=$((seen + 1))
        status="${line#${cap}: }"
        if [ "${status}" != "PASS" ]; then
            out="${out:+${out},}${cap}"
        fi
    done
    if [ "${seen}" -eq 0 ]; then
        echo "no capability lines in ${file}" >&2
        return 1
    fi
    printf '%s' "${out}"
}

# environment_id_from_doctor <doctor-output-file> prints the "전용 환경: <id>" value.
environment_id_from_doctor() {
    sed -n 's/^전용 환경: //p' "$1" | head -n 1
}

# requires_capability <problem-id> prints the capability a problem needs (if any).
requires_capability() {
    case "$1" in
        image-001) echo image-build ;;
        volume-001) echo storage ;;
        diagnostics-001) echo metrics ;;
        ingress-001) echo ingress ;;
        networkpolicy-001) echo network-policy ;;
        *) echo "" ;;
    esac
}

# release_environment <id> <state-dir> removes the throwaway environment that
# phase 2 created. Everything is matched by the exact environment ID.
release_environment() {
    local id="$1" state="$2" c
    [ -z "${id}" ] && return 0
    kind delete cluster --name "junkloud-${id}" --kubeconfig "${state}/environment/kubeconfig" >/dev/null 2>&1 || true
    for c in $(docker ps -aq --filter "label=junkloud.io/environment-id=${id}") \
             $(docker ps -aq --filter "label=io.x-k8s.cloud-provider-kind.cluster=junkloud-${id}"); do
        docker rm -f "${c}" >/dev/null 2>&1 || true
    done
}

secs() { # seconds -> 1h02m03s
    local s="$1"
    printf '%dh%02dm%02ds' $((s / 3600)) $(((s % 3600) / 60)) $((s % 60))
}

main() {
    local cold=0 skip_validate=0 given_unsupported=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --cold) cold=1 ;;
            --skip-validate) skip_validate=1 ;;
            --unsupported) shift; given_unsupported="${1:-}" ;;
            -h | --help)
                sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
                echo
                echo "Options:"
                echo "  --cold               ask the test to label the first start cold. Use it on a Docker that has"
                echo "                       never pulled the pinned images. Skips the doctor/validate phase (which would"
                echo "                       pull them); give the unsupported capabilities yourself with --unsupported."
                echo "  --unsupported LIST   comma-separated capabilities to treat as unsupported (only with --cold)."
                echo "  --skip-validate      skip the per-problem ckad bank validate loop."
                return 0
                ;;
            *) echo "unknown option: $1" >&2; return 2 ;;
        esac
        shift
    done

    cd "${repo_root}" || return 1
    local started evidence stamp work state
    started="$(date +%s)"
    stamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    evidence="${HOME}/junkloud-acceptance"
    mkdir -p "${evidence}"
    work="$(mktemp -d)"
    state="${work}/state"
    trap "rm -rf '${work}'" EXIT

    echo "== 1. Host facts (${stamp})"
    local arch memtotal ncpu dockerver
    arch="$(uname -m)"
    memtotal="$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)"
    ncpu="$(docker info --format '{{.NCPU}}' 2>/dev/null || echo '?')"
    dockerver="$(docker info --format '{{.ServerVersion}}' 2>/dev/null || echo '?')"
    local memgib
    memgib="$(awk -v b="${memtotal}" 'BEGIN { printf "%.2f", b / 1073741824 }')"
    echo "uname -m: ${arch}"
    echo "docker info MemTotal: ${memtotal} bytes (${memgib} GiB) -- the Docker Desktop VM memory"
    echo "docker info NCPU: ${ncpu}"
    echo "docker info ServerVersion: ${dockerver}"
    if [ "${memtotal}" = "0" ]; then
        echo "Docker is not reachable from this container. Nothing was run." >&2
        return 3
    fi

    local unsupported="" doctor_status="NOT RUN" env_id="" doctor_file="${work}/doctor.txt"
    local validate_rows="" validate_fail=0 validate_unexpected=0 validate_total=0 validate_status="NOT RUN"

    if [ "${cold}" -eq 1 ]; then
        unsupported="${given_unsupported}"
        doctor_status="NOT RUN (cold run: ckad doctor would pull the pinned images first; unsupported list given by hand: '${unsupported}')"
        validate_status="NOT RUN (cold run)"
    else
        echo
        echo "== 2. ckad doctor (throwaway state ${state})"
        # Only these commands see the throwaway directories; the shell's own
        # CKAD_* variables (if any) are left alone.
        CKAD_STATE_DIR="${state}" CKAD_WORKSPACE="${work}/ckad" ckad doctor 2>&1 | tee "${doctor_file}"
        local doctor_exit="${PIPESTATUS[0]}"
        env_id="$(environment_id_from_doctor "${doctor_file}")"
        if unsupported="$(unsupported_from_doctor "${doctor_file}")"; then
            if [ -z "${unsupported}" ]; then doctor_status="all capabilities PASS"; else doctor_status="FAIL: ${unsupported}"; fi
        else
            doctor_status="could not read the capability lines (doctor exit ${doctor_exit})"
            echo "ckad doctor did not report capabilities; stopping. Nothing further was run." >&2
            release_environment "${env_id}" "${state}"
            return 3
        fi

        if [ "${skip_validate}" -eq 1 ]; then
            validate_status="NOT RUN (--skip-validate)"
        else
            echo
            echo "== 3. ckad bank validate, one problem at a time"
            local id t0 t1 rc cap result
            for id in $(problem_ids); do
                validate_total=$((validate_total + 1))
                t0="$(date +%s)"
                CKAD_STATE_DIR="${state}" CKAD_WORKSPACE="${work}/ckad" ckad bank validate "${id}" >"${work}/validate-${id}.log" 2>&1
                rc=$?
                t1="$(date +%s)"
                cap="$(requires_capability "${id}")"
                if [ "${rc}" -eq 0 ]; then
                    result="PASS"
                elif [ -n "${cap}" ] && [[ ",${unsupported}," == *",${cap},"* ]]; then
                    result="FAIL (capability ${cap} unsupported on this host)"
                    validate_fail=$((validate_fail + 1))
                else
                    result="FAIL (exit ${rc})"
                    validate_fail=$((validate_fail + 1))
                    validate_unexpected=$((validate_unexpected + 1))
                fi
                cp "${work}/validate-${id}.log" "${evidence}/validate-${id}.log"
                echo "${id}: ${result} in $(secs $((t1 - t0)))"
                validate_rows="${validate_rows}| ${id} | ${result} | $(secs $((t1 - t0))) |
"
            done
            validate_status="${validate_total} problems, ${validate_fail} FAIL (${validate_unexpected} not explained by an unsupported capability)"
        fi
        echo
        echo "== 3b. removing the throwaway environment ${env_id}"
        release_environment "${env_id}" "${state}"
    fi

    echo
    echo "== 4. make test-acceptance (several hours)"
    export JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES="${unsupported}"
    export JUNKLOUD_ACCEPTANCE_EVIDENCE="${evidence}"
    [ "${cold}" -eq 1 ] && export JUNKLOUD_ACCEPTANCE_COLD=1
    echo "JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES='${JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES}'"
    echo "JUNKLOUD_ACCEPTANCE_EVIDENCE=${JUNKLOUD_ACCEPTANCE_EVIDENCE}"
    make test-acceptance 2>&1 | tee "${evidence}/acceptance.log"
    local acceptance_exit="${PIPESTATUS[0]}"

    local result_line partial_note
    result_line="$(grep -m1 'RESULT ' "${evidence}/acceptance.log" | sed 's/^[[:space:]]*//' || true)"
    [ -z "${result_line}" ] && result_line="no RESULT line in the log (the test did not finish)"
    if [ -n "${unsupported}" ]; then
        partial_note="PARTIAL: capability unsupported on this host: ${unsupported}. This was NOT the full mock-01."
    else
        partial_note="full mock-01 (no unsupported capability)"
    fi

    echo
    echo "== 5. Summary (paste this back)"
    echo "date: ${stamp}   total wall time: $(secs $(($(date +%s) - started)))"
    echo "| item | value |"
    echo "| --- | --- |"
    echo "| uname -m | ${arch} |"
    echo "| Docker VM memory | ${memgib} GiB (MemTotal ${memtotal} bytes) |"
    echo "| Docker CPUs | ${ncpu} |"
    echo "| Docker server | ${dockerver} |"
    echo "| ckad doctor | ${doctor_status} |"
    echo "| bank validate | ${validate_status} |"
    echo "| exam | ${partial_note} |"
    echo "| make test-acceptance exit | ${acceptance_exit} |"
    echo "| test result line | ${result_line} |"
    echo "| cold requested | $([ "${cold}" -eq 1 ] && echo yes || echo no) |"
    echo "| evidence | ${evidence} (cycle-N.json, acceptance.log, validate-*.log) |"
    if [ -n "${validate_rows}" ]; then
        echo
        echo "| problem | ckad bank validate | time |"
        echo "| --- | --- | --- |"
        printf '%s' "${validate_rows}"
    fi
    echo
    echo "Send back: this table, ${evidence}/acceptance.log and ${evidence}/cycle-*.json."
    echo "Nothing above is a result for a phase marked NOT RUN."

    if [ "${acceptance_exit}" -ne 0 ] || [ "${validate_unexpected}" -ne 0 ]; then
        return 1
    fi
    return 0
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
