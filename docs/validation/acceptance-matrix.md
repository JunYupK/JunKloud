# Acceptance matrix (Task 19)

What has been verified, where, and what has not. **Every cell is a result that was observed, or it says why it is not.** A test that cannot run is `NOT RUN` with the reason, never PASS. An emulator or sandbox result never fills an M2 or Windows cell. Device cells stay `미완료` until they are run on that device, and "출시 준비 완료" is declared only after the device runs (design spec section 13).

Legend: `PASS`/`FAIL` = observed, with commit, log and duration. `PARTIAL (19/20, network-policy unenforced)` = the exam was `PARTIAL: mock-01 minus [networkpolicy-001]` (19 problems, 92 points), never mock-01. `pending (...)` = not yet recorded, with the reason. `미완료` = not run on that device. `not run` = no run happened.

Evidence logs are in the local, uncommitted workspace `.superpowers/sdd/2026-11-03-task19-acceptance/` (`task-N-logs/`); they are not part of the repository. Base commit of the Task 4 runs: `11fbb1a` (`.../task-4-logs/base-commit.txt`).

Sandbox = Linux amd64, cgroup v1, IPv6 disabled in the engine kernel, NetworkPolicy **not enforced** (the kernel lacks `nft_queue`). Live runs use `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6 JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`.

## Matrix

| Item | sandbox linux/amd64 (cgroup v1) | CI ubuntu amd64/arm64 | M2 8GB / 4GiB VM | Windows 16GB |
| --- | --- | --- | --- | --- |
| Unit tests (`go test ./... -count=1`) | PASS at `11fbb1a`, every package ok (`task-4-logs/unit.log`) | not run (workflow defined, never triggered) | 미완료 | 미완료 |
| Race (`go test -race ./...`) | PASS at `11fbb1a`, 6m35s wall, every package ok (`task-4-logs/race.log`) | not run | 미완료 | 미완료 |
| Lint (`make lint`: vet for every tag, gofmt) | PASS at `11fbb1a`, exit 0 (`task-4-logs/lint.log`) | not run | 미완료 | 미완료 |
| CI workflow definition (`TestCIWorkflowParses`) and runbook script tests (`TestAcceptanceScript*`) | PASS (RED first: `ci-red.log`, `acceptance-script-red.log`; GREEN: `ci-green.log`, `acceptance-script-green.log`, part of `unit.log`) | not run | n/a | n/a |
| Native image build (`image` job, workspace and dev targets) | NOT RUN in Task 4 (only the CI definition is checked, by `TestCIWorkflowParses`) | not run | 미완료 | 미완료 |
| Integration banks, per domain (build, deployment, observability, security, networking) | pending (Task 5 records the suites run on unchanged content with commit and log and marks the rest NOT RUN; runs recorded in the Task 14-18 plans used earlier grader versions and do not count for `grader-9`) | not run (`integration-smoke` is dispatch-only) | 미완료 | 미완료 |
| `TestReadyFailedCreateIsLearnerFault` (grader-9 FailedCreate is the learner's FAIL) | PASS, 197.9 s, both fault subtests and the late-SA subtest at `65fae8c` (`task-1-logs/fix1-live.log`) | not run | 미완료 | 미완료 |
| Late-SA subtest, hardened (ERROR passes, PASS is inconclusive/skip, FAIL fails) | PASS: subtest `late-sa-is-stale-error` 25.35 s (ERROR naming the backing-off controller), whole `TestReadyFailedCreateIsLearnerFault` 171.4 s, on the tree containing `11fbb1a` (`task-4-logs/late-sa-live.log`; RED first: `late-sa-red.log`). A PASS verdict would have been skipped as inconclusive, not counted | not run | 미완료 | 미완료 |
| `TestCLIFlow` (CLI over a real cluster) | PASS, `ok` 542.6 s (`task-2-logs/fix1-e2e.log`, Task 2 fix round; the log does not record the commit) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `KillDuringPrepare` | pending (Task 3 not run yet) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `InterruptDuringGrade` | pending (Task 3 not run yet) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `KillDuringReset` | pending (Task 3 not run yet) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `APIOutageDuringGrade` | pending (Task 3 not run yet) | not run | 미완료 | 미완료 |
| `TestInterruptedOperations`: `ConcurrentCommands` | pending (Task 3 not run yet) | not run | 미완료 | 미완료 |
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

Note a. The lifecycle and `TestCLIFlow` cells come from Task 2's fix round (spec-order test). An earlier run at `47a8a15` used an order that the review rejected and is not the result of record. The `TestInterruptedOperations` and integration-bank cells are filled by Task 3 and Task 5 from their finished logs, with commit, log, duration and the exact result line.

The lifecycle evidence above was produced at `f2fb82c`, before `b40a736` removed the `batch-001` Job TTL. `task-2-logs/fix2-batch001.log` is the separate live run of `batch-001` after that change: subtests `smoke-batch-001` and `batch-001` PASS (478.6 s), but the log's parent `TestBuildBank` line reads FAIL and its reason was not diagnosed in Task 4, so treat the revalidation as unconfirmed until Task 5 reads it. The lifecycle numbers are from a bank that differs from HEAD only in `batch-001`; they are not a re-run on HEAD.

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
