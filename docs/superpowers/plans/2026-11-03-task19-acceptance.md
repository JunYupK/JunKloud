# Task 19 Acceptance, Recovery and Usage Documentation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Tasks 14–18 are merged (master `618bfc4`): the bank is 20 problems / 100 points and `exams/mock-01.yaml` exists.

**Goal:** This plan closes the original plan's Task 19:
- fix the one known grader-8 gap (`grader-9`);
- prove the full mock lifecycle, recovery after real process interruption, and no leaks after three cycles, with real CLI processes on real kind clusters;
- add CI;
- write the usage, authoring and acceptance documents, so that the user can run the device validation (M2 8GB, Windows 16GB) and record it.

**Architecture:**
- Product code changes in Task 1, the grader `ready` cause classification plus the `GraderVersion` bump. Other product changes are allowed only as minimal fixes that a new failing test proves, made in the module that owns the failure.
- New tests live in `tests/e2e/`:
  - `acceptance_test.go` (build tags `linux && acceptance`);
  - `recovery_test.go` (build tags `linux && e2e`);
  - shared helpers in `harness_test.go` and `observe_test.go` (build tags `linux && (e2e || acceptance)`).
- They drive the built `ckad` binary in subprocesses, as `TestCLIFlow` does (`tests/e2e/cli_flow_test.go:126-152`).

**Tech Stack:** Existing pinned Go, client-go, kind v0.33 / Kubernetes v1.37.0, Docker CLI (`docker stats`, `docker inspect`), GitHub Actions (`ubuntu-24.04`, `ubuntu-24.04-arm`).

**Spec:** `docs/superpowers/specs/2026-10-14-ckad-training-system-design.md`:
- §4.3 lines 103-107;
- §11;
- §12;
- §13 (the table, lines 367, 371 and 373).

The original Task 19 is in `docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md:467-484`.

## Global Constraints

- "기본 세트에 대해 준비→모범 풀이→채점→reset을 최소 3회 연속 수행해 결과 일치, 잔여 문제 리소스·데이터 없음, 명백한 자원 누적 없음, OOM·환경 원인의 Pending 없음과 개인 파일 보존을 확인한다. 장애를 심는 테스트는 별도로 수행한다." (spec §13 L371)
- "시간 측정은 최초 다운로드가 포함된 cold start와 이미지가 준비된 warm start를 구분한다. warm start, 문제별·전체 reset, 전체 grade의 시간과 peak 메모리를 기록한다. … 특정 초 단위 속도를 보장하지 않는다. M2 8GB·4GiB VM 예산을 통과하지 못하면 완료로 표시하지 않는다." (spec §13 L373)
- "메모리 부족 시 문제를 몰래 생략하거나 점수를 재분배하지 않는다." (spec §4.3 L106)
- "프로세스 중단 후 다음 명령은 미완료 작업을 감지하고 복구 방법을 안내한다. 누락된 namespace·클러스터를 답안과 함께 자동으로 새로 만들지 않는다." (spec §11)
- 소유하지 않은 클러스터 객체·Docker 대상·개인 파일은 보존한다 (spec §12).
- A test that cannot run in its environment reports SKIPPED/NOT RUN with the reason, never PASS. An emulator or sandbox result never fills an M2 or Windows cell.
- Every device cell the controller cannot reach stays `미완료` (not run) in the acceptance matrix. "출시 준비 완료" (ready for release) is reported only after the user's device runs.
- The sandbox does not enforce NetworkPolicy (`JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`). It runs with `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6`. After a container restart, run `rm -f /var/run/docker.pid; nohup dockerd >/tmp/dockerd.log 2>&1 &`.
- Carried rules: wrong cases are server-accepted states; only `alternative-*` cases score full marks; never print a Secret value; Korean feedback, English questions.
- Commit trailers: `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_015FJ16SY7LYBGvR2bWTs4vL`. Nothing is pushed by implementers.

## Review Focus

1. **A process killed mid-operation.** Killing `start`, `grade` or `reset` part-way must not leave a state where the next command neither explains the problem nor offers a way out. Expect `status` or `start` to name the interrupted operation and the command that recovers it (Task 3, `TestInterruptedOperations`).
2. **Kubernetes API down during grading.** If the API becomes unreachable during `grade`, the run must produce ERROR with no confirmed score, never FAIL. `grade --retry` after recovery must complete the same submission (Task 3, subtest `APIOutageDuringGrade`).
3. **Reset after a completed mock.** It must leave no owned objects behind: namespaces, cluster-scoped objects, PV backing paths, owned images or open operations. It must also keep foreign objects and personal files byte-identical (Task 2, `NoLeaksAfterThreeCycles`).
4. **Memory pressure while preparing the whole mock.** If memory runs short during prepare, the result must surface as an environment failure. It must not appear as a learner FAIL or as silently dropped problems. The lifecycle test asserts no OOMKilled container and no Pending Pod with `FailedScheduling`, and records the peak (Task 2).
5. **grader-9 misclassifying infrastructure failures.** A webhook outage or an internal API error must stay ERROR. Only admission rejections of the learner's own spec may become FAIL (Task 1 table test).

---

### Task 1: grader-9 — a learner-caused FailedCreate is FAIL

**Files:**
- Modify: `internal/grader/pods.go:67-80` (`classifyRollout`), `internal/grader/ready.go:226-252`, `internal/grader/version.go:26`
- Test: `internal/grader/ready_test.go`, `tests/integration/resource_grading_test.go`
- Records: `docs/superpowers/plans/2026-11-02-task17-environment-security.md` (the known-limitation paragraph near :316) and the original plan's Task 17 record (:445). Mark the limitation "resolved in grader-9" for Deployments and ReplicaSets.

**Interfaces:**
- Produces:
  - `const GraderVersion = "grader-9"`;
  - `func createFailure(obj object) (learner bool, message string)` in `pods.go`.
- `createFailure` reads `status.conditions[type=ReplicaFailure, status=True, reason=FailedCreate].message` from a Deployment or ReplicaSet object.
- `classifyRollout` gains a parameter, `classifyRollout(pods []object, expectPods bool, workload object, cfg causeConfig) rolloutCause`. `ready.go` passes the observed workload object.

- [ ] **Step 1: Write the failing unit tests** in `ready_test.go`. Extend the existing no-pods table (`ready_test.go:283`):
  ```go
  {"quota exceeded", deploymentWithReplicaFailure(`pods "web-5f6" is forbidden: exceeded quota: team-quota, requested: requests.memory=512Mi, used: requests.memory=0, limited: requests.memory=256Mi`), "FAIL", "exceeded quota"},
  {"service account missing", deploymentWithReplicaFailure(`pods "web-5f6" is forbidden: error looking up service account shop/report-readr: serviceaccount "report-readr" not found`), "FAIL", "serviceaccount"},
  {"pod security", deploymentWithReplicaFailure(`pods "web-5f6" is forbidden: violates PodSecurity "restricted:latest": runAsNonRoot != true`), "FAIL", "violates PodSecurity"},
  {"limit range", deploymentWithReplicaFailure(`pods "web-5f6" is forbidden: maximum memory usage per Container is 256Mi, but limit is 512Mi`), "FAIL", "maximum memory"},
  {"webhook down", deploymentWithReplicaFailure(`Internal error occurred: failed calling webhook "x.example": connection refused`), "ERROR", "failed calling webhook"},
  {"no condition", nil, "ERROR", "created no pods"},
  ```
  - A message counts as learner fault only when it contains `is forbidden:` and contains none of `failed calling webhook`, `Internal error`, `connection refused`, `context deadline exceeded`, `the server is currently unable`.
  - The FAIL reason must contain the shortened message. Use the existing `shorten`.
  - Also add a StatefulSet row whose ReplicaFailure-like text appears only in events. It stays ERROR, so the scope stays Deployment/ReplicaSet.
- [ ] **Step 2:** Run `go test ./internal/grader -run TestReady -count=1`. Expect FAIL on the four new FAIL rows.
- [ ] **Step 3: Implement** `createFailure` and the extra `classifyRollout` argument. Bump `GraderVersion` to `grader-9`. Update the doc comment at `ready.go:37-40`.
- [ ] **Step 4: Write the live integration test.** In `tests/integration/resource_grading_test.go`, add `TestReadyFailedCreateIsLearnerFault`. It creates two Deployments in a test namespace:
  - one with `serviceAccountName: missing-sa`;
  - one with a ResourceQuota `requests.memory: 64Mi` and a Pod requesting 128Mi.

  Each `ready` criterion (replicas 1, timeout 30s) must give FAIL with a reason containing `serviceaccount` or `exceeded quota`.
- [ ] **Step 5:** Run:
  - `go test ./... -count=1`;
  - `go vet ./...`;
  - `go vet -tags=integration ./...`;
  - `gofmt -l .` (empty);
  - with the sandbox env, `go test -tags=integration ./tests/integration -run TestReadyFailedCreateIsLearnerFault -count=1 -v`.

  All must pass. Then grep the bank's expected vectors (`problems/*/problem.yaml`) for `"ERROR"`; there must be none. That shows no certified vector can change. Log the grep result.
- [ ] **Step 6: Commit** `fix: classify a learner-caused FailedCreate as FAIL (grader-9)`, together with the record edits.

### Task 2: Full mock lifecycle with three cycles and no leaks

**Files:**
- Create:
  - `tests/e2e/harness_test.go`: binary build, the `run` helper and cleanup, moved from `cli_flow_test.go`, which then uses them;
  - `tests/e2e/observe_test.go`: leak, sentinel, memory and OOM observers;
  - `tests/e2e/acceptance_test.go`.
- Modify: `tests/e2e/cli_flow_test.go` (use the shared helpers; behaviour unchanged), `Makefile`.

**Interfaces (Task 3 reuses these):**
- `type cliHarness struct{ t *testing.T; bin, state, workspace, bank string; env []string }`
- `func newHarness(t *testing.T, bank string) *cliHarness`
  - builds `./cmd/ckad`;
  - sets `CKAD_STATE_DIR`, `CKAD_WORKSPACE`, `CKAD_BANK_DIR`, `CKAD_HOST_COMPAT` (from `JUNKLOUD_TEST_HOST_COMPAT`) and the decoy `KUBECONFIG`;
  - registers the platform-stop and kind-delete cleanup (`cli_flow_test.go:92-125`).
- `func (h *cliHarness) run(stdin string, wantExit int, args ...string) (stdout, stderr string)`
- `func (h *cliHarness) start(args ...string) *exec.Cmd`: an asynchronous run, for kills.
- `func (h *cliHarness) solveCurrent(ctx context.Context)`: applies every frozen problem's `solution` actions for the current session through `author.NewActionExecutorWithTools(...).Execute` into the problem's latest attempt dir.
  - Exception: `image` build actions run as `ckad image build --problem Qn --context <ctx> --tag <ref>`, so that reset owns the image (`internal/environment/cleanup_images.go:58-63`).
  - `image save` runs plain `docker save --output <path> <ref>`, as the learner would.
- `type ownedLeftovers struct{ Namespaces, ClusterObjects, VolumePaths, Images, OpenOperations []string }`
- `func observeLeftovers(ctx context.Context, t *testing.T, h *cliHarness) ownedLeftovers`, which checks:
  - every namespaced and cluster-scoped listable resource (dynamic discovery) carrying label `junkloud.io/session-id`;
  - PV hostPath backings that still exist in the node container;
  - Docker images labelled `junkloud.io/session-id` or `junkloud.io/image-build`;
  - operations whose phase is not `done`.
- `type foreignSentinel struct{ NamespaceUID, ConfigMapData, ImageID string }`
  - `func plantForeign(ctx, t, h) foreignSentinel` plants a namespace `foreign-acceptance-sentinel` with ConfigMap `keep`, plus the Docker tag `foreign.local/sentinel:keep`.
  - `func readForeign(ctx, t, h) foreignSentinel` reads them back.
- `func personalFiles(t *testing.T, root string) map[string]string`: path → sha256 for `<workspace>/.bashrc` and every file the test writes under attempt dirs.
- `type memorySampler struct{...}`
  - `func startMemorySampler(ctx context.Context, interval time.Duration) *memorySampler` samples `docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}'` for kind node, cloud-provider-kind and envoy containers.
  - `Stop() (peakTotalMiB int, peakByContainer map[string]int)`.
- `func oomOrPending(ctx, t, h) []string`: containers with `State.OOMKilled=true`, Pods with an `OOMKilled` last state, and Pods Pending with a `FailedScheduling` event.
- `type cycleEvidence struct{ Cycle int; Exam string; PrepareStart, Grade, Reset time.Duration; InitialScore, SolvedScore, Total int; PeakMiB int; Leftovers ownedLeftovers; OOMOrPending []string }`, written as JSON to `$JUNKLOUD_ACCEPTANCE_EVIDENCE` (default `t.TempDir()`). Each file's path is logged.

**Exam selection (no silent substitution):**
- The test reads `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES`.
- If it is empty, the test serves `mock-01` from `/src` unchanged.
- Otherwise the test copies the bank into a temp dir (`copyDir`, `tests/integration/session_start_test.go:234-255`). It writes `exams/mock-<n>.yaml`, where n is the number of remaining problems: mock-01's list, in order, minus every problem whose `capabilities` names an unsupported capability.
- Every evidence record and the final `t.Log` then say `PARTIAL: mock-01 minus [<ids>] (capability <name> unsupported on this host)`. The test never reports that run as mock-01.

**Certificates:**
- The test's own temp store gets stand-in validation records, as `tests/integration/mock_serving_test.go:57-71` does.
- They are labelled in a comment and in the evidence as `stand-in certificates: lifecycle evidence, not package validation`.
- Package validation evidence comes from the bank tests and from `ckad bank validate` (documented in Task 4).

- [ ] **Step 1: Write the failing test** `TestFullMockLifecycle` in `acceptance_test.go`:
  1. Preflight. Before cycle 1, plant the foreign sentinel and the personal files `.bashrc` and `notes.txt`.
  2. Run three cycles. Each cycle runs:
     - `start <exam>` with `yes\n`, exit 0;
     - write the personal file `attempt/answer-notes.txt` in Q1's latest attempt;
     - `grade` (exit 1), whose total must equal the sum of the problems' `initial_score`;
     - `solveCurrent`;
     - `grade --resubmit` (exit 0), printing `점수: <total>/<total>`;
     - `review` (exit 0);
     - `reset`, with the environment ID read from `ckad status` on stdin (exit 0).
  3. Assertions (subtest `NoLeaksAfterThreeCycles` holds the leak assertions):
     ```go
     testutil.Equal(t, solvedScores, []int{total, total, total})
     testutil.Equal(t, initialScores, []int{initial, initial, initial})
     testutil.Equal(t, leftovers.Count(), 0)           // after every reset
     testutil.Equal(t, oomOrPendingFound, []string(nil))
     testutil.Equal(t, readForeign(ctx, t, h), sentinelBefore)
     testutil.Equal(t, personalFiles(t, ws), personalBefore)
     ```
     Also assert that namespace and PV counts after each reset equal the count recorded before cycle 1. This is the "no obvious accumulation" requirement. Peak memory is recorded, not asserted.
  4. Timing:
     - cycle 1's `start` is the cold start when `JUNKLOUD_ACCEPTANCE_COLD=1` and the image cache was removed beforehand;
     - otherwise every cycle is warm;
     - the evidence records which.
- [ ] **Step 2:** Run `go vet -tags=acceptance ./tests/e2e/` and `go test -tags=acceptance ./tests/e2e -run TestFullMockLifecycle -count=1` without Docker running. Expect FAIL with a missing-tool error, never a skip-as-pass. Then confirm `go test -tags=e2e ./tests/e2e -run TestCLIFlow` still compiles, and `go vet -tags=e2e ./tests/e2e/` too.
- [ ] **Step 3: Implement the helpers** listed in Interfaces. Refactor `cli_flow_test.go` onto them; it must keep its 29/29 assertion.
- [ ] **Step 4: Live run in the sandbox.** The run takes about 3 h. Use the sandbox env and `JUNKLOUD_ACCEPTANCE_EVIDENCE=.superpowers/sdd/<ws>/task-2-logs/evidence`:
  ```
  go test -tags=acceptance ./tests/e2e -run TestFullMockLifecycle -count=1 -v -timeout=6h
  ```
  Expected: PASS for `PARTIAL: mock-01 minus [networkpolicy-001]`, 19 problems, 92 points. Save the log.
  - If a product defect appears, stop. Write a failing test in the owning module, fix it minimally there, and report it as a finding.
  - Never weaken an assertion.
- [ ] **Step 5:** Run `make test-e2e` with the sandbox env (TestCLIFlow). It must PASS.
- [ ] **Step 6:** Update the Makefile:
  - `test-acceptance: go test -tags=acceptance ./tests/e2e/... -count=1 -v -timeout=6h`;
  - `lint: go vet ./... && go vet -tags=integration ./... && go vet -tags=e2e ./tests/e2e/ && go vet -tags=acceptance ./tests/e2e/ && test -z "$$(gofmt -l .)"`.
- [ ] **Step 7: Commit** `test: verify the full mock lifecycle over three cycles`.

### Task 3: Real interruption and recovery

**Files:**
- Create: `tests/e2e/recovery_test.go` (build tags `linux && e2e`)
- Fixtures: the dev bank `tests/fixtures/dev-bank`, which is fast.

**Interfaces:**
- Consumes `newHarness`, `run`, `start`, `observeLeftovers` and `personalFiles` from Task 2.
- Produces `func waitOperationPhase(ctx context.Context, t *testing.T, h *cliHarness, kind, phase string) model.Operation`. It polls the state store read-only, opened per poll and closed again, every 100ms with a 5-minute limit.

- [ ] **Step 1: Write the failing test** `TestInterruptedOperations`. Each subtest starts from a certified dev bank in a fresh state dir. They share one cluster, created once.

  | Subtest | How | What the next commands must do |
  |---|---|---|
  | `KillDuringPrepare` | `start --mode practice --count 5 --seed 42`; SIGKILL when op `session-prepare` reaches `setup-applied` | `status` (exit 3) names the interrupted prepare and suggests `ckad reset`. A new `start` is refused with exit 3, and no missing namespace is recreated. `reset` with the env ID gives exit 0. `observeLeftovers` is 0. A new `start` then succeeds. |
  | `InterruptDuringGrade` | `grade` sent SIGINT once op `session-grade` is `planned` | `grade` (plain) gives exit 3 with `ErrGradeInterrupted` text. `grade --retry` gives exit 1 or 0 with a confirmed score and the same submission (one submission row). |
  | `KillDuringReset` | `reset` killed when op `session-reset` is `planned` or `deleted` | `start` gives exit 3 with "초기화가 중단되었습니다". `reset --recover` gives exit 0. `observeLeftovers` is 0. Personal files are unchanged. |
  | `APIOutageDuringGrade` | `docker pause <control-plane>` just after `grade` starts, `docker unpause` after it exits | That `grade` gives exit 3 with no `점수:` line and no FAIL counted as a score. After unpause, `grade --retry` completes. |
  | `ConcurrentCommands` | two simultaneous `grade` runs | one succeeds; the other gives exit 3 with the lock message; one grading run is recorded. |

  - Where the current CLI wording is unknown, assert the exit code plus a stable substring that the implementer reads from `internal/cli/root.go:100-151`.
  - If the product does not meet a "must" in the table, write a failing unit test in the owning package and make a minimal fix. Two examples: `status` does not mention an interrupted prepare; `reset` refuses a SETUP_ERROR session.
  - Each such fix is reported as a finding with its file.
- [ ] **Step 2:** Run the test with the sandbox env, `go test -tags=e2e ./tests/e2e -run TestInterruptedOperations -count=1 -v -timeout=40m`. Record which subtests fail before any fix (RED).
- [ ] **Step 3: Implement** only the fixes the RED proved.
- [ ] **Step 4:** Re-run until PASS. Then run `go test ./... -count=1` and `make lint`.
- [ ] **Step 5:** In the Makefile, change `test-e2e` to `-timeout=60m`, because TestCLIFlow plus recovery runs longer.
- [ ] **Step 6: Commit** `test: recover from interrupted prepare, grade and reset`, together with any fix commits, each with its own regression test.

### Task 4: CI, usage and authoring documents, and the acceptance matrix

**Files:**
- Create:
  - `.github/workflows/ci.yaml`
  - `docs/usage/mac.md`
  - `docs/usage/windows.md`
  - `docs/usage/training.md`
  - `docs/authoring/problem-guide.md`
  - `docs/validation/acceptance-matrix.md`
  - `runtime/acceptance.sh`
- Modify: `README.md` (current status, links, developer targets), `runtime/README.md` (link only).

**Interfaces:**
- `runtime/acceptance.sh`:
  - runs inside the dev container (`bash runtime/up.sh dev`);
  - prints the host facts: `uname -m`, `docker info` MemTotal / NCPU / ServerVersion, and the Docker Desktop VM memory;
  - runs `ckad doctor`, then `ckad bank validate` for all 20 IDs, timing each;
  - then runs `make test-acceptance` with `JUNKLOUD_ACCEPTANCE_EVIDENCE=$HOME/junkloud-acceptance` (cold start if `--cold`);
  - prints a summary table the user can paste back.
  - It never sets `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES`. A host that fails `network-policy` therefore runs the partial mock, and the summary says so.

- [ ] **Step 1: CI workflow.** Jobs:
  - `static` (`ubuntu-24.04`): `go test ./... -count=1`, `make lint`, `go build ./cmd/ckad` for `GOARCH=amd64` and `arm64`;
  - `image`: `docker build` of the runtime Dockerfile on `ubuntu-24.04` and `ubuntu-24.04-arm`. These are native runners, not emulation;
  - `integration-smoke`: `workflow_dispatch` only. It installs kind and runs `go test -tags=integration ./tests/integration -run 'TestNetworkBankSmoke|TestReadyFailedCreateIsLearnerFault' -count=1 -timeout=60m`;
  - `validate-networkpolicy`: `workflow_dispatch` only. It runs `TestNetworkBank/networkpolicy-001` with `-timeout=90m` and uploads the log. If the runner enforces NetworkPolicy, this can give the first real certification evidence. Its result is recorded only from the actual run log.

  Verification: `go run` a small YAML parse with `sigs.k8s.io/yaml`, already a dependency, as `TestCIWorkflowParses` in `tests/runtime`. It asserts that the job names exist and that no job uses QEMU/`setup-qemu-action`, because M2 results must not come from emulation.
- [ ] **Step 2: Usage documents** (Korean):
  - `mac.md` and `windows.md`: Docker Desktop settings (VM memory 4GiB as the validation budget, Linux containers), `up.sh` / `up.ps1`, a second terminal, the `k` alias and completion, the Docker socket's privilege scope, and the exclusion of SSH and multiple contexts.
  - `training.md`: practice, mock, show, grade, `--retry` / `--resubmit`, review, reset (problem, full, cluster, recover), retry of wrong answers, and error recovery. Every command is checked against `ckad --help` output, which is captured in the task log.
- [ ] **Step 3:** Write `problem-guide.md` (English or Korean, matching the existing `docs/author-tool-actions.md`). It covers:
  - the package layout and criteria kinds;
  - the carried authoring rules from the Task 14–18 plans: "do not change X" sentences are graded; wrong cases are server-accepted states; `depends_on` instead of ERROR; `delete-owned` before `apply`; stand-in vs real certificates; Secret-reading criteria for redaction;
  - the `ckad bank validate` cycle.
- [ ] **Step 4:** Write `acceptance-matrix.md`.
  - Rows:
    - unit / race / lint;
    - integration banks (per domain);
    - TestCLIFlow;
    - TestInterruptedOperations (per subtest);
    - TestFullMockLifecycle (cycles, solved / initial scores, leftovers, OOM/Pending, peak memory, warm-start, grade and reset times);
    - networkpolicy-001 certification;
    - Ingress address wait;
    - 4GiB fit.
  - Columns: `sandbox linux/amd64 (cgroup v1)`, `CI ubuntu amd64/arm64`, `M2 8GB / 4GiB VM`, `Windows 16GB`.
  - Fill the sandbox column only from Task 2/3 logs, citing commit, log path, duration and exact result. Mark the partial-mock cells `PARTIAL (19/20, network-policy unenforced)`.
  - Fill CI cells only if a run happened. The M2 and Windows columns are `미완료`.
  - Under the table, put a device runbook: `runtime/acceptance.sh` and what to send back.
- [ ] **Step 5: README.** Update "현재 상태와 제한" to state:
  - grader-9;
  - the lifecycle and recovery evidence and their limits;
  - that the device validation is still to be done.

  Add links to the new documents and the `make lint` / `test-acceptance` targets.
- [ ] **Step 6:** Run `go test ./... -count=1` and `make lint`. Then run `bash -n runtime/acceptance.sh` and `shellcheck` if it is available. Commit `docs: add usage, authoring and acceptance documents and CI`.

### Task 5: Records and final verification

**Files:**
- Modify: the original plan's Task 19 record and checkboxes (`2026-10-14-ckad-training-system-implementation.md:467-484`), and this plan's verified-evidence section.

- [ ] **Step 1:** Run `make test` and `make test-race`. Then `make lint`. Then `make test-integration` with the sandbox env. The full run takes several hours. Run it, or record precisely which suites ran in Tasks 1–3 and in the earlier bank tasks on unchanged content, and mark the rest NOT RUN with the reason. Then `make test-e2e`. Record each command's exact result.
- [ ] **Step 2:** Write the 실행 기록 (execution record) under the original Task 19. It covers:
  - commits;
  - grader-9;
  - the lifecycle evidence (PARTIAL mock, 3 cycles, numbers);
  - the recovery subtests;
  - CI status;
  - NOT RUN items: M2, Windows, mock-01 positive start, networkpolicy-001 certification and 4GiB fit.
  - "출시 준비 완료" is withheld until the device cells are filled.
- [ ] **Step 3: Commit** `test: verify full mock lifecycle and document operation`. This is the original plan's commit name.

## Verified evidence

Logs are in the local, uncommitted workspace `.superpowers/sdd/2026-11-03-task19-acceptance/task-N-logs/`. Sandbox env for live runs: `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6 JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`. Full table: `docs/validation/acceptance-matrix.md`.

| Item | Result | Commit | Log |
| --- | --- | --- | --- |
| grader-9 unit tests (RED then GREEN) | PASS; 6 stale-guard rows were RED first | `f603b72`, `65fae8c` | task-1-logs/red.log, fix1-red.log, fix1-green.log |
| `TestReadyFailedCreateIsLearnerFault` live | PASS 197.9 s (missing-sa, quota, late-sa) | `65fae8c` | task-1-logs/fix1-live.log |
| Late-SA subtest hardened (ERROR passes, PASS skipped) | PASS 166.6 s, subtest 34.2 s | tree after `8d8cb4e` | task-4-logs/fix1-late-sa-live.log |
| `TestFullMockLifecycle` (PARTIAL: mock-01 minus [networkpolicy-001], 19 problems, 92 points, stand-in certificates) | PASS 4569.9 s; 3 cycles; initial [20 20 20]; solved [92 92 92]; 0 leftovers; no OOM/Pending; peak 1158/1189/1210 MiB; warm-after-setup | `f2fb82c` | task-2-logs/fix1-live.log, evidence-fix1/ |
| batch-001 after TTL removal | subtest PASS 478.64 s, seeds [1 3], hash `4a009b41...`; parent FAIL is the `-run` filter artefact | `b40a736` | task-2-logs/fix2-batch001.log |
| `TestInterruptedOperations` | 5/5 PASS 952.27 s | Task 3 fix round 1, before `c6f3b5a` | task-3-logs/fix1-interrupted.log |
| `TestCLIFlow` | PASS 497.34 s | Task 3 fix round 1 | task-3-logs/fix1-cliflow.log |
| `make test-e2e` | PASS 1594.69 s; pause-retry path never exercised (an earlier run failed on `docker pause`) | `c6f3b5a` | task-3-logs/fix1-make-test-e2e-2.log, fix1-make-test-e2e.log |
| `make test` | exit 0, 14 packages ok | `c6f3b5a` + docs | task-5-logs/make-test.log |
| `make test-race` | exit 0, 14 packages ok | `c6f3b5a` + docs | task-5-logs/make-test-race.log |
| `make lint` | exit 0 | `c6f3b5a` + docs | task-5-logs/make-lint.log |
| README/docs vs `--help`, links and anchors | 0 problems (help output identical to Task 4's) | Task 5 docs | task-5-logs/help-*.log, links-help-check.log |
| Problem content since the last bank runs | only batch-001 changed (revalidated) | | task-5-logs/problems-unchanged-check.log |

NOT RUN: full `make test-integration` (ruling R7); a bank re-run under grader-9; Apple M2 8GB/4GiB; Windows 16GB; GitHub CI; `runtime/acceptance.sh` on a real device; mock-01 positive start; networkpolicy-001 certification; 4GiB fit; cold start. Device cells stay `미완료`. "출시 준비 완료" (ready for release) is withheld until the user's device runs.

Known product gaps recorded for the user (not changed): managed `ckad image build` is refused after a COMPLETED session; the grade fingerprint hashes whole Deployment specs, so an HPA scale during grading of `api-migration-001` could give a spurious `!inputs-changed` ERROR (M9); the `ckad status` lock probe writes `env.lock`; `start` explicit-flag detection (`NFlag()`) is always 0 (M5).

### Final fix wave (BASE `dbe592b`)

Commits (local): `6983084` (start explicit-flag detection via `Flag.Changed`, SETUP_ERROR message, grade `--retry` hint only for incomplete results), `0bb0436` (`lock.Held` read-only probe, race close), `cb070eb` (acceptance context 5h40m; unpause always, wait node and pods Ready), `b3da763` (compose-label preflight, `--unsupported` needs `--cold`), `0d4b591` (message wording), plus the docs commit. Each product change had a failing unit test first (`final-fix-logs/item1-3-red.log`, `item2-red.log`, `item2-session-red.log`, `item5-red.log`).

| Item | Result | Log |
| --- | --- | --- |
| `go test ./... -count=1` | PASS (all packages ok) | final-fix-logs/unit.log |
| `make lint` | exit 0 | final-fix-logs/lint.log |
| `make test-race` | see final-fix-report.md | final-fix-logs/race.log |
| `TestCLIFlow` live | PASS 562.86 s | final-fix-logs/live.log |
| `TestInterruptedOperations` live | PASS 1092.68 s; KillDuringPrepare 129.61, InterruptDuringGrade 107.83, KillDuringReset 85.41, APIOutageDuringGrade 276.51, ConcurrentCommands 103.55 s. The pause-retry path was exercised once by a real refusal ("unable to freeze", attempt 1) | final-fix-logs/live.log |
| README-vs-help and links/anchors checkers | 0 problems; fresh `--help` identical to Task 5 except the environment-dependent `--state-dir` default | final-fix-logs/links-help-check.log, final-fix-logs/help/ |

Kept recorded, not fixed: M9 (a grader-10 follow-up), M-l, a managed image build after COMPLETED, test hygiene. Device cells stay 미완료.
