# Task 16 Observability & Maintenance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add and genuinely validate `api-migration-001`, `probes-001` and `diagnostics-001` (5 points each, 15 total, domain `observability`), raising the partial bank from 9/40 to 12/55.

**Architecture:** Content-only: every check uses existing registered criterion kinds (`file`, `field`, `resource`, `ready`, `logs`, `http`) and existing author actions (`apply`, `file`, `delete-owned`). No new checker, action, schema field or `GraderVersion` bump (stays `grader-8`). If a package genuinely cannot be expressed with them, stop and report BLOCKED with the exact gap instead of adding grader code.

**Tech Stack:** Existing pinned Go, client-go, kind v0.33 / Kubernetes v1.37.0, vendored metrics-server v0.9.0, provided `docker.io/library/busybox:1.37.0`, declarative JSON-style `problem.yaml` packages.

**Spec:** `docs/superpowers/specs/2026-10-14-ckad-training-system-design.md` (§3.1 line 58, §6–§8); original Task 16 in `docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md` (lines 409–425, bank table §4).

## Global Constraints

- 초기 검증 기준은 **kind v0.33 / Kubernetes v1.37**. `latest`를 쓰지 않는다.
- 기본 모의시험: **20문제, 120분, 100점**, domain 점수 **20/20/15/25/20**.
- 일반 훈련 세트 **5~20개**, 오답 재도전 **1~20개**.
- **문제·요구사항은 영어, 해설·오답 피드백은 한국어**.
- criterion 결과는 **PASS / FAIL / ERROR**. ERROR가 있으면 확정 총점 없음. 초기·정답·대표 오답·reset 검증을 통과한 문제만 출제한다.
- 일반 수렴 대기 기본 **30초**, 네트워크 개별 timeout **3초**, 최대 **3회**. 긴 관찰이 필요한 문제는 명시적 override를 정의한다.
- 사용자에게 요구하지 않은 조건을 숨은 채점 기준으로 추가하지 않는다 (spec §6.1). Every criterion maps to a sentence in `question.en.md`.
- CLI 명령 사용 이력을 강제 채점하지 않는다. API migration은 현재 v1.37에서 실제 받아들여지는 객체를 검사한다. probe 테스트는 플랫폼을 흔드는 무차별 kill을 쓰지 않는다 (plan Task 16).
- 사용자가 지정한 잘못된 이미지·selector·probe 때문에 앱이 실패한 것은 FAIL이다 (spec §8.1).
- Workloads use only the provided `docker.io/library/busybox:1.37.0` with the same `imagePullPolicy` existing packages use; small `budget`; two `namespace` parameter values per package (`jk-<id>-a`, `jk-<id>-b`) so `author.CoveringSeeds` yields ≥2 seeds.
- Private `file` action sources are copied raw (not parameter-rendered); `setup/provided/` text files and `apply` manifests are rendered. Keep `${namespace}` out of private file sources.
- Sandbox integration env (this cloud VM, see `.superpowers/sdd/.../sandbox-env.md`): `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6 JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`. Native M2/4GiB/Windows are NOT RUN.

## Review Focus

1. A symptom-hiding answer (probes deleted, command replaced by `sleep`, value hard-coded) must lose points even though the workload looks Ready — each package has a case proving it (Tasks 2, 3).
2. A semantically equivalent answer (named port, `matchExpressions` selector, ConfigMap `items` mapping instead of key rename, extra HPA `behavior`) must get full marks — each package has an `alternative-*` case at 5/5 (Tasks 1–3).
3. "Fixed the file but never applied it" and "applied something else than the file" are distinguishable: file criteria and live criteria are separate points (Task 1).
4. A liveness/startup misconfiguration that restarts pods must be FAIL (learner fault) on every run, not ERROR or a lucky PASS — relies on the grader-2 stability window; the initial broken state is validated on both seeds (Task 2).
5. Adding the observability coverage group must not break existing build/deployment coverage assertions; global totals become 12/55 with no duplicate IDs (Task 4).

## File map

| Task | Creates | Modifies |
| --- | --- | --- |
| 1 | `problems/api-migration-001/**`, `tests/integration/bank_observability_content_test.go`, `tests/integration/bank_observability_test.go` | — |
| 2 | `problems/probes-001/**` | content test case table |
| 3 | `problems/diagnostics-001/**` | content test case table |
| 4 | — | `curriculum/coverage.yaml`, `tests/integration/bank_deployment_content_test.go` (`TestDeploymentCoverage`), original plan Task 16 section, this plan's evidence section |

Each package: `problem.yaml` (JSON style like `problems/rollout-001/problem.yaml`), `question.en.md`, `explanation.ko.md` (concept, diagnosis order, solution, official doc links), `setup/`, `setup/provided/` when files are handed to the learner, `solution/`, `cases/`. `type`, `difficulty`, `tags`, `competencies`, `sources` follow existing packages; competencies use the spec §3.1 wording: `API deprecation`, `probes and health checks`, `CLI monitoring`, `logs`, `application debugging`.

---

### Task 1: Bank test scaffolding and `api-migration-001`

**Files:**
- Create: `tests/integration/bank_observability_content_test.go` (`//go:build linux`), `tests/integration/bank_observability_test.go` (`//go:build integration`)
- Create: `problems/api-migration-001/{problem.yaml,question.en.md,explanation.ko.md,setup/service.yaml,setup/provided/README.txt,setup/provided/legacy/app.yaml,solution/app.yaml,cases/*}`

**Interfaces:**
- Consumes: `loadGroup`, `sumPoints`, `preflightBank` (`bank_helpers_test.go`); `openBankRuntime`, `smokeBank`, `validateGroupAfterSmoke` (`bank_build_test.go`); `author.CoveringSeeds`.
- Produces: `var observabilityIDs = []string{"api-migration-001", "probes-001", "diagnostics-001"}`; `observabilityCases map[string]map[string]int` (problem ID → case ID → expected score) that Tasks 2–3 extend; `TestObservabilityProblems` (per-ID subtests), `TestObservabilityBankPreflight`, `TestObservabilityBankSmoke`, `TestObservabilityBank`.

Package design (pinned):
- `setup/provided/legacy/app.yaml` (no `namespace:` field): `extensions/v1beta1` Deployment `inventory` (replicas 2, no `spec.selector`, template labels `app: inventory`, `tier: api`, busybox container `web` writing `inventory-ok` to `/www/index.html` then `exec httpd -f -p 8080 -h /www`) and `autoscaling/v2beta1` HorizontalPodAutoscaler `inventory` (`scaleTargetRef` Deployment `inventory`, min 2, max 4, `metrics: [{type: Resource, resource: {name: cpu, targetAverageUtilization: 70}}]`). Both GVs are unserved on v1.37.
- `setup/service.yaml` (applied): Service `inventory`, selector `app: inventory, tier: api`, port 80 → targetPort 8080.
- Question: update `legacy/app.yaml` in place so each object uses a served API (Deployment `apps/v1`, HPA `autoscaling/v2`), keeping names, replicas 2, labels, bounds 2–4 and the 70% CPU utilization target; apply it to namespace `${namespace}`; Service `inventory` must serve the app.
- Criteria (1 point each, `feedback` in Korean):
  1. `manifest-deployment` — `file` `legacy/app.yaml` yaml, `select {api_version: apps/v1, kind: Deployment, name: inventory}`, fields `spec.selector` exists, `spec.replicas` equals 2.
  2. `manifest-hpa` — `file` same path, `select {api_version: autoscaling/v2, kind: HorizontalPodAutoscaler, name: inventory}`, fields `spec.metrics[type=Resource].resource.target.type` equals `Utilization`, `...target.averageUtilization` equals 70.
  3. `deployment-ready` — `ready` apps/v1 Deployment `inventory`, replicas 2.
  4. `hpa-live` — `field` autoscaling/v2 HPA `inventory`: `scaleTargetRef.name` inventory, `minReplicas` 2, `maxReplicas` 4, metric target averageUtilization 70.
  5. `service-serves` — `http` via Service `inventory` port 80, body marker `inventory-ok`, `depends_on: [deployment-ready]`.
- Initial: all FAIL, `initial_score` 0.
- Solution: `file` (`solution/app.yaml` → `legacy/app.yaml`) then `apply` `solution/app.yaml`.
- Cases (expected score): `apiversion-only` (file action writes a manifest with only apiVersion lines changed; nothing applied because the server rejects it) → 0; `fixed-not-applied` → 2; `wrong-hpa-target` (`averageValue`/utilization 50, file and live) → 3; `selector-drops-tier` (apps/v1 with labels/selector `app: inventory` only; Service has no endpoints) → 4; `alternative-match-expressions` (`matchExpressions` selector + HPA `behavior`) → 5.

- [ ] **Step 1: Write the failing tests.** `TestObservabilityProblems` runs a subtest per ID present in `observabilityCases` asserting:
  ```go
  testutil.Equal(t, p.Domain, model.DomainObservability)
  testutil.Equal(t, sumPoints([]model.Problem{p}), 5)
  testutil.Equal(t, len(seeds) >= 2, true)
  for id, want := range observabilityCases[p.ID] { testutil.Equal(t, caseScore(p, id), want) } // every listed case exists with that expected_score
  testutil.Equal(t, initialBelowMax(p), true)
  ```
  plus question/asset presence as in `TestDeploymentBankContent`. `TestObservabilityBankPreflight` runs `preflightBank` over the present IDs. Seed `observabilityCases` with the five api-migration cases above.
- [ ] **Step 2: Run RED.** `go test ./tests/integration -run 'TestObservability(Problems|BankPreflight)' -count=1 -v` → FAIL loading `problems/api-migration-001` (missing package).
- [ ] **Step 3: Author the package** per the design above.
- [ ] **Step 4: Static GREEN.** Same command → PASS; `go vet -tags=integration ./tests/integration` clean.
- [ ] **Step 5: Live smoke then validation.** Write `TestObservabilityBankSmoke` / `TestObservabilityBank` mirroring `bank_deployment_test.go` (`validateGroupAfterSmoke(t, loadGroup(t, observabilityIDs), false)`), loading only IDs whose package exists until Task 4 removes that guard. Run with the sandbox env: `go test -tags=integration ./tests/integration -run '^TestObservabilityBankSmoke$' -count=1 -timeout=20m -v`, then `-run '^TestObservabilityBank$/^api-migration-001$' -timeout=40m`. Expected: PASS, every seed's initial/solved/each case/reset stage logged. Fix the package (never the expected values to fit a wrong grader) on failure; keep failed logs as evidence.
- [ ] **Step 6: Commit** `content: add validated api migration problem`.

### Task 2: `probes-001`

**Files:** Create `problems/probes-001/{problem.yaml,question.en.md,explanation.ko.md,setup/checkout.yaml,solution/checkout.yaml,cases/*}`; Modify `observabilityCases`.

**Interfaces:** Consumes Task 1 test helpers and `observabilityCases`.

Package design (pinned):
- App: Deployment `checkout` replicas 2, container `web`: `sleep 10`, then writes `checkout-ok` to `/www/index.html`, `ok` to `/www/healthz` and `/www/ready`, then `exec httpd -f -p 8080 -h /www`. Service `checkout` port 80 → 8080 (in setup).
- Broken setup: readiness `httpGet /ready port 8081`; liveness `httpGet /health port 8080, periodSeconds 2, failureThreshold 1`; no startupProbe → pods restart-loop and are never Ready.
- Question: readiness HTTP GET `/ready` on the app port; liveness HTTP GET `/healthz` on the app port; add a startupProbe HTTP GET `/healthz` that tolerates the ~10s startup; do not change image/command; both replicas must stay Ready without restarts and Service `checkout` must serve the app.
- Criteria (1 point each): `readiness-probe` (`field` `spec.template.spec.containers[name=web].readinessProbe.httpGet.path` equals `/ready`), `liveness-probe` (`...livenessProbe.httpGet.path` equals `/healthz`), `startup-probe` (`...startupProbe.httpGet.path` equals `/healthz`), `rollout-stable` (`ready` replicas 2, `stable_for` `"15s"`, timeout `"90s"`), `service-serves` (`http` Service `checkout`, marker `checkout-ok`, depends on `rollout-stable`). Ports are proven by behavior, not by field equality, so `port: http` and `port: 8080` are both valid.
- Initial: `readiness-probe` PASS (path already `/ready`, wrong port), rest FAIL → `initial_score` 1.
- Cases: `probes-deleted` (all probes removed) → 2; `readiness-port-kept` (8081 kept, others fixed) → 3; `liveness-delay-no-startup` (liveness `initialDelaySeconds: 15`, no startupProbe) → 4; `alternative-named-port` (container port named `http`, probes use `port: http`, startup `periodSeconds 5 failureThreshold 10`) → 5.

- [ ] **Step 1: RED.** Add the four cases to `observabilityCases["probes-001"]`; run `go test ./tests/integration -run 'TestObservabilityProblems/probes-001' -count=1 -v` → FAIL (package missing).
- [ ] **Step 2: Author the package**; static GREEN with the same command plus `TestObservabilityBankPreflight`.
- [ ] **Step 3: Determinism check.** Live `-run '^TestObservabilityBank$/^probes-001$' -timeout=40m` PASS; in the log, the initial and `probes-deleted` stages on both seeds show `rollout-stable` FAIL resp. PASS with a learner-fault cause (not ERROR). Any ERROR on a broken-probe stage is a finding to report, not to mask with a longer timeout.
- [ ] **Step 4: Commit** `content: add validated probes problem`.

### Task 3: `diagnostics-001`

**Files:** Create `problems/diagnostics-001/{problem.yaml,question.en.md,explanation.ko.md,setup/orders.yaml,setup/workers.yaml,setup/provided/README.txt,solution/*,cases/*}`; Modify `observabilityCases`.

**Interfaces:** Consumes Task 1 test helpers; `capabilities: ["metrics"]` (`environment.CapabilityMetrics`).

Package design (pinned):
- `orders`: ConfigMap `orders-config` with key `orders.conf` = `endpoint=orders-db.${namespace}.svc:5432`; Deployment `orders` replicas 1 mounting the ConfigMap at `/etc/orders`; container `app` command reads `/etc/orders/app.conf`; if absent it logs `FATAL: /etc/orders/app.conf not found (mounted: <ls output>)` and exits 1 (CrashLoop); if present it logs `orders started <endpoint line>`, writes `orders-ok <endpoint line>` to `/www/index.html` and `exec httpd -f -p 8080 -h /www`. Service `orders` port 80 → 8080.
- Workers: bare Pods `ledger-worker`, `audit-worker`, `report-worker`; only `report-worker` runs a busy loop (cpu limit 200m), the others sleep. Fixed names across seeds (private file sources are not rendered; the namespace varies per seed).
- Question: find why `orders` is not running and fix the root cause without changing its image, command or args; `orders` must be Ready, log its start line and serve through Service `orders`; write the name of the Pod in the namespace using the most CPU to `answers/top-cpu.txt`.
- Criteria (1 point each): `top-cpu-answer` (`file` `answers/top-cpu.txt` text, `matches` `^\s*report-worker\s*$`), `command-unchanged` (`field` `containers[name=app].command` equals the setup list and `image` equals the provided image), `orders-ready` (`ready` replicas 1), `orders-started` (`logs` Deployment `orders`, container `app`, `matches` `orders started endpoint=orders-db\.`, depends on `orders-ready`), `service-serves` (`http` Service `orders`, marker `orders-ok`, depends on `orders-ready`).
- Initial: `command-unchanged` PASS → `initial_score` 1.
- Solution: re-apply `orders-config` with key `app.conf` (same value); `file` writes `answers/top-cpu.txt`.
- Cases: `sleep-instead` (command replaced by `sleep 3600`) → 1; `answer-only` → 2; `hardcoded-endpoint` (command echoes the endpoint itself, ConfigMap untouched, answer written) → 4; `wrong-pod-answer` (fixed, answer `ledger-worker`) → 4; `alternative-items-mapping` (Deployment volume `items: [{key: orders.conf, path: app.conf}]`, answer written) → 5.

- [ ] **Step 1: RED.** Add the five cases; `go test ./tests/integration -run 'TestObservabilityProblems/diagnostics-001' -count=1 -v` → FAIL (package missing).
- [ ] **Step 2: Author the package**; static GREEN incl. preflight.
- [ ] **Step 3: Metrics reality check.** In the live run log (or a one-off `ckad doctor` on the test environment) metrics capability is Available and `kubectl top pod -n <ns>` lists `report-worker` highest; record the output. If metrics is unavailable in the sandbox, the problem must be refused by serving (capability gate) — record NOT RUN for the learner-side observation, never fake it.
- [ ] **Step 4: Live** `-run '^TestObservabilityBank$/^diagnostics-001$' -timeout=40m` PASS on both seeds.
- [ ] **Step 5: Commit** `content: add validated diagnostics problem`.

### Task 4: Group totals, coverage, full bank run and records

**Files:** Modify `curriculum/coverage.yaml`, `tests/integration/bank_observability_content_test.go`, `tests/integration/bank_deployment_content_test.go` (`TestDeploymentCoverage`), `docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md` (Task 16 section), this plan (Verified evidence).

**Interfaces:** Consumes all three packages.

- [ ] **Step 1: RED.** Add `TestObservabilityBankContent` (group: `len == 3`, `sumPoints == 15`, all IDs present — remove the "present IDs only" guard from Task 1) and `TestObservabilityCoverage` (exactly one group `{id: observability, domain: observability, count: 3, points: 15, problems: observabilityIDs}`); change `TestDeploymentCoverage`'s global assertion to count 12 / points 55 / no duplicate IDs across groups. Run `go test ./tests/integration -run 'Coverage|BankContent' -count=1 -v` → FAIL (no observability group).
- [ ] **Step 2: GREEN.** Append the observability group to `curriculum/coverage.yaml` without touching existing groups; rerun → PASS.
- [ ] **Step 3: Full live run** once on the frozen tree: `go test -tags=integration ./tests/integration -run '^TestObservabilityBank$' -count=1 -timeout=90m -v` → PASS, then `go test ./... -count=1`, `go vet ./...`, `go vet -tags=integration ./...`, `gofmt -l .` clean. Record SHA, durations, seeds and stage counts; per-problem runs from Tasks 1–3 count only if the tree is unchanged since.
- [ ] **Step 4: Records.** Tick Task 16 in the original plan with an 실행 기록 paragraph (same style as Task 15: SHA, run times, 12/55 partial bank, mock still incomplete, NOT RUN items) and fill this plan's Verified evidence. Commit `docs: record validated observability milestone`. Leave nothing running.

## Verified evidence

(Filled in by Task 4.)
