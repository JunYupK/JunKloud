# Acceptance matrix (Task 19)

What has been verified, where, and what has not. **Every cell is a result that was observed, or it says why it is not.** A test that cannot run is `NOT RUN` with the reason, never PASS. An emulator or sandbox result never fills an M2 or Windows cell. Device cells stay `미완료` until they are run on that device, and "출시 준비 완료" is declared only after the device runs (design spec section 13).

Legend: `PASS`/`FAIL` = observed, with commit, log and duration. `PARTIAL (19/20, network-policy unenforced)` = the exam was `PARTIAL: mock-01 minus [networkpolicy-001]` (19 problems, 92 points), never mock-01. `pending (...)` = not yet recorded, with the reason. `미완료` = not run on that device. `not run` = no run happened.

Evidence logs are in the local, uncommitted workspace `.superpowers/sdd/2026-11-03-task19-acceptance/` (`task-N-logs/`); they are not part of the repository. Base commit of the Task 4 runs: `11fbb1a` (`.../task-4-logs/base-commit.txt`). Task 5 runs (unit, race, lint) are at `c6f3b5a` plus documentation-only changes (`.../task-5-logs/base-commit.txt`).

Sandbox = Linux amd64, cgroup v1, IPv6 disabled in the engine kernel, NetworkPolicy **not enforced** (the kernel lacks `nft_queue`). Live runs use `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6 JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`.

## Matrix

| Item | sandbox linux/amd64 (cgroup v1) | CI ubuntu amd64/arm64 | M2 8GB / 4GiB VM | Windows 16GB |
| --- | --- | --- | --- | --- |
| Unit tests (`go test ./... -count=1`) | PASS at `c6f3b5a`, 14 packages ok, exit 0 (`make test`, `task-5-logs/make-test.log`); earlier PASS at `11fbb1a` (`task-4-logs/unit.log`) | not run (workflow defined, never triggered) | 미완료 | 미완료 |
| Race (`go test -race ./...`) | PASS at `c6f3b5a`, 14 packages ok, exit 0 (`make test-race`, `task-5-logs/make-test-race.log`); earlier PASS at `11fbb1a`, 6m35s (`task-4-logs/race.log`) | not run | 미완료 | 미완료 |
| Lint (`make lint`: vet for every tag, gofmt) | PASS at `c6f3b5a`, exit 0 (`make lint`, `task-5-logs/make-lint.log`); earlier PASS at `11fbb1a` (`task-4-logs/lint.log`) | not run | 미완료 | 미완료 |
| CI workflow definition (`TestCIWorkflowParses`) and runbook script tests (`TestAcceptanceScript*`) | PASS (RED first: `ci-red.log`, `acceptance-script-red.log`; GREEN: `ci-green.log`, `acceptance-script-green.log`, part of `unit.log`) | not run | n/a | n/a |
| Native image build (`image` job, workspace and dev targets) | NOT RUN in Task 4 (only the CI definition is checked, by `TestCIWorkflowParses`) | not run | 미완료 | 미완료 |
| Integration banks, per domain (build, deployment, observability, security, networking) | Full `make test-integration` **NOT RUN** in Task 5 (user ruling R7: it takes many hours; the device runs and the earlier per-domain runs stand). Runs that exist, all with grader versions before `grader-9` (record in the Task 14-18 plans, logs not in this workspace): build `TestBuildBank` at `085401f` (five subtests PASS, parent FAIL at the practice-5 gate, recorded as such; `image-001` re-validated at `0bf9a9c`; build packages identical between the two); deployment `TestDeploymentBank` 3478.45 s at `7bef126`; observability `TestObservabilityBank` 4731.22 s at `4f7fdf6`, `api-migration-001` again 767.93 s at `69c78e5`; security `TestSecurityBank` 4091.75 s at `168c372`; networking `TestNetworkBank` 3665.06 s at `0a7d12c` (`service-001`, `ingress-001` PASS; `networkpolicy-001` FAIL, NetworkPolicy unenforced). Problem content unchanged since those runs except `batch-001` (`task-5-logs/problems-unchanged-check.log`; `networkpolicy-001` changed once at `6d0fc3c`, smoke only). `batch-001` revalidated below. NOT RUN: a full bank re-run under `grader-9`; the grader-9 change is covered by unit tests and `TestReadyFailedCreateIsLearnerFault`, and no expected vector contains ERROR (`task-1-logs/grep-error.log`) | not run (`integration-smoke` is dispatch-only) | 미완료 | 미완료 |
| `batch-001` revalidation after the Job TTL removal (`b40a736`) | subtest `TestBuildBank/batch-001` PASS 478.64 s and `smoke-batch-001` PASS 10.07 s, seeds [1 3], hash `sha256:4a009b41e9cb0b5122842b358ecf2888d81d88b1780491c0dadc008f45e89a38`, vectors unchanged (`task-2-logs/fix2-batch001.log`). The parent `TestBuildBank` line is FAIL only because of the `-run` filter: its final practice-5 CLI step needs all five build problems certified ("fewer problems than requested", `bank_build_test.go:232`). Whole five-problem build bank not re-run | not run | 미완료 | 미완료 |
| `TestReadyFailedCreateIsLearnerFault` (grader-9 FailedCreate is the learner's FAIL) | PASS, 197.9 s, both fault subtests and the late-SA subtest at `65fae8c` (`task-1-logs/fix1-live.log`) | not run | 미완료 | 미완료 |
| Late-SA subtest, hardened (ERROR passes, PASS is inconclusive/skip, FAIL fails) | PASS: subtest `late-sa-is-stale-error` 25.35 s (ERROR naming the backing-off controller), whole `TestReadyFailedCreateIsLearnerFault` 171.4 s, on the tree containing `11fbb1a` (`task-4-logs/late-sa-live.log`; RED first: `late-sa-red.log`). A PASS verdict would have been skipped as inconclusive, not counted | not run | 미완료 | 미완료 |
| `TestCLIFlow` (CLI over a real cluster) | PASS 497.34 s after Task 3's product changes (`task-3-logs/fix1-cliflow.log`, Task 3 fix round 1); earlier PASS 542.6 s (`task-2-logs/fix1-e2e.log`) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `KillDuringPrepare` | PASS 130.36 s: `status` exit 3 `이전 준비가 중단되었습니다...`; `start` refused; `reset` then cleans up (`task-3-logs/fix1-interrupted.log`, run in Task 3 fix round 1 before the pause-retry commit; the log does not record its commit) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `InterruptDuringGrade` | PASS 109.91 s: next plain `grade` exit 3 `채점을 확정할 수 없습니다. 사용할 수 있는 명령: ckad grade --retry / ckad grade --resubmit` (`task-3-logs/fix1-interrupted.log`, run in Task 3 fix round 1 before the pause-retry commit; the log does not record its commit) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `KillDuringReset` | PASS 81.67 s: `start` exit 3 `초기화가 중단되었습니다`, `reset --recover` completes, no leftovers (`task-3-logs/fix1-interrupted.log`, run in Task 3 fix round 1 before the pause-retry commit; the log does not record its commit) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `APIOutageDuringGrade` | PASS 133.09 s: solved answer, outage grade exit 3, no `: FAIL (` and no `점수:` line; `grade --retry` exit 0 `점수: 29/29` (`task-3-logs/fix1-interrupted.log`, run in Task 3 fix round 1 before the pause-retry commit; the log does not record its commit) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `ConcurrentCommands` | PASS 101.08 s: one command exit 0 (29/29), the other exit 3 with the lock message; one submission (`task-3-logs/fix1-interrupted.log`, run in Task 3 fix round 1 before the pause-retry commit; the log does not record its commit) | not run | 미완료 | 미완료 |
| Whole `make test-e2e` (`TestCLIFlow`, `TestInterruptedOperations`, others) | PASS at `c6f3b5a`: `ok ... 1594.690s`, `make test-e2e exit=0` (`task-3-logs/fix1-make-test-e2e-2.log`, non-verbose, so no per-subtest lines). An earlier run FAILED (`fix1-make-test-e2e.log`): `docker pause` was refused by the kernel ("unable to freeze") in `APIOutageDuringGrade`, then `ConcurrentCommands` cascaded; a pause retry was added in `c6f3b5a`. **The pause-retry path was never exercised by a refusal**; the passing run simply did not hit it. HEAD product code equals `c6f3b5a`, so it was not re-run | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: 3 cycles result | PARTIAL (19/20, network-policy unenforced): PASS 4569.9 s, `RESULT PARTIAL: mock-01 minus [networkpolicy-001] ...: 3 cycles, scores [92 92 92] of 92` (`task-2-logs/fix1-live.log`, commit under test `f2fb82c`) | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: initial and solved scores | PARTIAL (19/20): initial [20 20 20] (from prepare verification), solved [92 92 92] of 92 (`fix1-live.log` line 1256; `evidence-fix1/cycle-*.json`) | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: leftovers after reset (namespaces, cluster objects, volumes, images, open operations) | PARTIAL (19/20): 0 in all 3 cycles; namespaces after 8 (= baseline), volumes 0 (`evidence-fix1/cycle-*.json`) | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: OOM / environment-caused Pending | PARTIAL (19/20): none in all 3 cycles (`fix1-live.log` cycle lines, `oom/pending 0`) | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: personal files and foreign objects preserved | PARTIAL (19/20): subtest `NoLeaksAfterThreeCycles` PASS (`fix1-live.log`) | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: peak memory | PARTIAL (19/20): 1158 / 1189 / 1210 MiB (node about 1100-1153 MiB); recorded, not asserted; says nothing about a 4GiB VM | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: warm start, grade and reset times | PARTIAL (19/20), warm-after-setup: start 7m26s / 7m24s / 7m24s; solved grade 4m37s / 5m10s / 4m42s; reset 12m29s / 12m26s / 12m25s; environment setup 72 s (`fix1-live.log` cycle lines) | not run | 미완료 | 미완료 |
| `TestFullMockLifecycle`: cold start time | NOT RUN (no cold start was measured; images were cached) | not run | 미완료 | 미완료 |
| `networkpolicy-001` certification (`ckad bank validate`) | NOT CERTIFIED: this kernel does not enforce NetworkPolicy, so its solved stage cannot pass here (Task 18 record, `docs/superpowers/plans/2026-11-02-task18-networking-mock.md`). Never marked validated from the sandbox | not run (`validate-networkpolicy` is dispatch-only; if the runner enforces NetworkPolicy it can give the first real evidence, and its result is recorded only from the actual run log) | 미완료 | 미완료 |
| Positive `mock-01` start (all 20 served) | NOT RUN (needs a host where `ckad doctor` reports `network-policy` PASS). The sandbox only proved the refusal (Task 18 record) | not run | 미완료 | 미완료 |
| Ingress address wait (grader waits at most 15 s, `internal/grader/network_eval.go`) | about 3 s in the sandbox (Task 18 record, `ingress-001`); one data point only | not run | 미완료 (measure the real wait before relying on 15 s) | 미완료 (same) |
| 4GiB Docker VM fit | NOT ASSESSED: the sandbox has no 4GiB VM. The summed declared budget of `mock-01` is 3744 MiB (3.66 GiB), a sum of declared budgets and not a measured peak (Task 18 record) | not run | 미완료 | n/a (Windows target is 16GB) |

Note a. The lifecycle cells come from Task 2's fix round (spec-order test). An earlier run at `47a8a15` used an order that the review rejected and is not the result of record. The `TestInterruptedOperations` cells are from Task 3 fix round 1; the integration-bank cells are filled by Task 5 as described in their row.

The lifecycle evidence was produced at `f2fb82c`, before `b40a736` removed the `batch-001` Job TTL and before Task 3's CLI changes (status/start/grade wording and exit codes). It was **not re-run on HEAD**; the bank differs from HEAD only in `batch-001`, whose revalidation is the `batch-001` row above.

Known limits that apply to all sandbox cells:

- A sandbox result says nothing about Apple M2 (ARM64) or Windows. The sandbox peak memory covers only that host's kind node and helpers and is not evidence for a 4GiB VM.
- The lifecycle certificates are stand-ins (current problem hash, grader version, environment hash). They are lifecycle evidence, not package validation.
- The lifecycle exam is PARTIAL on any host that fails `network-policy`; it is labelled that way and never reported as mock-01.
- The lifecycle test records peak memory but does not assert it.
- Full reset of the 19-problem session took about 12.5 minutes (reset column above) (namespaces are deleted one at a time). The spec promises no particular speed.

## Device runbook (M2, Windows)

Run on the real device, inside the dev container. It takes several hours.

```sh
git clone https://github.com/JunYupK/JunKloud.git && cd JunKloud
bash runtime/up.sh dev              # Windows PowerShell: powershell -ExecutionPolicy Bypass -File runtime/up.ps1 -Service dev
docker compose exec dev bash
bash runtime/acceptance.sh          # warm run
```

Before the first run on a device:

1. Set the Docker VM memory to 4GiB (the validation budget) and note the setting you used.
2. **Stop your own JunKloud cluster, and nothing else heavy may run.** The lifecycle test sums the memory of every `junkloud-*`, `kindccm-*` and `envoy` container, so an idle cluster from your own practice would contaminate the peak and put two clusters into one small VM. `runtime/acceptance.sh` therefore refuses to start while such a container is running (the work containers `junkloud-dev-1` and `junkloud-workspace-1` are excluded). Stop them without removing anything:

   ```sh
   docker stop $(docker ps -q --filter "name=^junkloud-[0-9a-f]{12}-" --filter "name=^junkloud-cpk-" --filter "name=^kindccm-")
   ```

   Your state volume, answers and the stopped cluster containers are kept. After the run, `docker start` the same containers and check with `ckad doctor`. Whether stopped nodes come back depends on Docker; see the README FAQ.

What `runtime/acceptance.sh` does:

1. Prints `uname -m`, and `docker info` `MemTotal`, `NCPU`, `ServerVersion` (the MemTotal is the Docker Desktop VM memory).
2. Refuses to start next to another JunKloud environment (step 2 above), then runs `ckad doctor` and `ckad bank validate` for every problem of the bank (20 today), timing each, in a throwaway state directory. Your own practice state, kubeconfig and solutions are not touched, and the validation records live only in that throwaway state, so before practising you still run the README section 4 validation loop in your own state. The throwaway environment is removed, matched by its exact environment ID, also after Ctrl+C.
3. Exports `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy` only when `ckad doctor` reports `network-policy` FAIL. The acceptance test then runs a labelled PARTIAL exam (19 problems) and the summary shows the test's own `RESULT` line, cross-checked against the doctor result. Any other capability FAIL is UNEXPECTED: the script stops with a non-zero exit, because the product requires those capabilities and a full mock-01 cannot be served without them. It never sets the variable by hand (except `--unsupported network-policy` in the `--cold` mode).
4. Runs `make test-acceptance` with `JUNKLOUD_ACCEPTANCE_EVIDENCE=$HOME/junkloud-acceptance`.
5. Prints a summary table.

Options: `--skip-validate` (skip the per-problem validation loop); `--cold` for a cold-start timing. A cold run must be the first thing run on a Docker that has never pulled the pinned images, and `ckad doctor` would pull them, so `--cold` skips phases 2-3 and takes the unsupported capability from `--unsupported network-policy` (the only accepted value). The preflight still applies. Do the cold run once, then a normal warm run.

**What to send back:**

- the summary table that the script prints;
- `$HOME/junkloud-acceptance/acceptance.log`;
- `$HOME/junkloud-acceptance/cycle-1.json`, `cycle-2.json`, `cycle-3.json`;
- `$HOME/junkloud-acceptance/validate-*.log` (the per-problem validation output of the script run);
- the Docker Desktop memory setting you used and the exact device (model and RAM, or Windows build and RAM).

The controller copies observed values into the device columns above. If a phase shows `NOT RUN`, that cell stays `미완료`. If `ckad doctor` reports `network-policy` FAIL on the device, the run is PARTIAL and the `networkpolicy-001` and positive mock-01 cells stay open. If it reports PASS, the script's own `validate-networkpolicy-001.log` and its timed row in the summary are the certification evidence for the device; record the result from that log (the script's `ckad doctor` check only proves ingress deny, so also read the criteria results in the log). No separate re-run is needed unless that log is missing or the run was interrupted.
