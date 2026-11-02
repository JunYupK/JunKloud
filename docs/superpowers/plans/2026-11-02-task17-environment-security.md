# Task 17 Environment / Configuration / Security Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add and genuinely validate `custom-resource-001`, `access-001`, `resources-001`, `configuration-001` and `security-001` (5 points each, 25 total, domain `environment-security`), raising the partial bank from 12/55 to 17/80.

**Architecture:** Mostly content. Grading uses existing checkers: `resource`, `field`, `ready`, `logs`, `http`, `file`, `authorization` (SubjectAccessReview / TokenReview) and `security-context` (declared and exec behaviour). One product change is allowed. After applying a CustomResourceDefinition, the environment waits until it is `Established`, so a later stage can apply custom resources of that kind without a NoMatch race. No new criterion kind, schema field or capability. `GraderVersion` stays `grader-8`.

**Tech Stack:** Existing pinned Go, client-go, kind v0.33 / Kubernetes v1.37.0 (Pod Security Admission built in), provided `docker.io/library/busybox:1.37.0`, JSON-style `problem.yaml` packages.

**Spec:** `docs/superpowers/specs/2026-10-14-ckad-training-system-design.md` (§3.1 line 59, §6–§8); original Task 17 and bank table in `docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md` (§4, Task 17). Pattern: `docs/superpowers/plans/2026-10-23-task16-observability.md`.

## Global Constraints

- 초기 검증 기준은 **kind v0.33 / Kubernetes v1.37**. `latest`를 쓰지 않는다.
- 기본 모의시험: **20문제, 120분, 100점**, domain 점수 **20/20/15/25/20**. 이 Task가 Environment/Configuration/Security **25점**을 채운다.
- **문제·요구사항은 영어, 해설·오답 피드백은 한국어**.
- criterion 결과는 **PASS / FAIL / ERROR**. ERROR가 있으면 확정 총점 없음. 초기·정답·대표 오답·reset 검증을 통과한 문제만 출제한다.
- 사용자에게 요구하지 않은 조건을 숨은 채점 기준으로 추가하지 않는다 (spec §6.1). Every criterion maps to a sentence in `question.en.md`. A sentence that says "do not change X" is graded, or it is not written.
- least privilege가 채점 조건이면 허용·불허 행동을 영어로 명시한다. CRD fixture는 관리자가 제공하고 명시 소유권으로 reset한다. namespace admission 정책과 workload SecurityContext를 함께 검증해 정책 제거 우회를 막는다. 원문 Secret을 실패 메시지에 포함하지 않는다 (plan Task 17).
- Build, Modify, Troubleshoot, Behavioral 유형을 섞고 기본 시험에 네 유형을 모두 포함한다 (spec §7.2). The bank has no `behavioral` problem yet, so this task adds one (`security-001`). It also mixes difficulty `easy`/`medium`/`hard`.
- Workloads use only the provided `docker.io/library/busybox:1.37.0` with the same `imagePullPolicy` existing packages use. Each problem has a small `budget` and two `namespace` parameter values (`jk-<id>-a`, `jk-<id>-b`) so `author.CoveringSeeds` gives ≥2 seeds.
- Rulings carried over from Task 16:
  - every `http` criterion has timeout `"60s"`;
  - private `file` sources contain no `${`;
  - a case that removes fields from a setup-created object uses `delete-owned` and then `apply` (Create-owned fields cannot be removed by a later server-side apply);
  - only `alternative-*` cases may score full marks;
  - stability windows and timeouts are stated in the question.
- Wrong cases must be states the API server accepts (a removed label, a loosened policy, a deleted quota). An action that admission or quota rejects (403/422) aborts validation, so it never belongs in a case.
- Never let `ready` meet a state where admission or quota blocks Pod creation: "no pods" is ERROR. Give such `ready` and `http` criteria a `depends_on` on a `field` or `security-context` (declared) criterion that fails first, so that they FAIL as "prerequisite not satisfied".
- Learner-created cluster-scoped objects are not owned and survive reset. The questions say to use namespaced objects only (except the provided CRD).
- Sandbox integration env: `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6 JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`. Native M2/4GiB/Windows are NOT RUN.

## Review Focus

1. Over-privilege must lose points even when everything works: a RoleBinding to `edit` or a ClusterRoleBinding must FAIL `least-privilege` while the app still runs (Task 3).
2. Bypassing admission must lose points even when the workload becomes Ready: removing or weakening the namespace `pod-security.kubernetes.io/enforce` label must FAIL (Task 6).
3. Secret values never appear in any evaluation evidence, reason or CLI grade text, for the correct, hard-coded and wrong-key cases (Task 5).
4. Quota/admission-blocked Pods yield FAIL (prerequisite), never ERROR, on every seed (Tasks 4, 6).
5. Semantically equivalent answers get full marks: `0.1` vs `100m` CPU, pod-level vs container-level securityContext, split Roles, CR fields in any key order (Tasks 2–6).

## File map

| Task | Creates | Modifies |
| --- | --- | --- |
| 1 | `tests/integration/bank_security_content_test.go` (linux), `tests/integration/bank_security_test.go` (integration) | `internal/environment/apply.go` (+ test), `tests/integration/bank_helpers_test.go` (`preflightBank` registers security checkers), `tests/integration/bank_build_test.go` (`smokeBank` environment-security branch) |
| 2 | `problems/custom-resource-001/**` | content test case table |
| 3 | `problems/access-001/**` | content test case table |
| 4 | `problems/resources-001/**` | content test case table |
| 5 | `problems/configuration-001/**` | content test case table, secret-leak assertion |
| 6 | `problems/security-001/**` | content test case table |
| 7 | — | `curriculum/coverage.yaml`, `tests/integration/bank_deployment_content_test.go` (`TestDeploymentCoverage` 17/80/17), `README.md` + `runtime/README.md` bank lists, original plan Task 17 section, this plan's evidence |

Competency strings (the 8 official items of spec §3.1, used exactly):
1. `CRD and Operator resources`
2. `authentication and authorization`
3. `admission control`
4. `requests, limits and quotas`
5. `ConfigMap`
6. `Secret`
7. `ServiceAccount`
8. `SecurityContext and capabilities`

Task 7 checks that each string is used by at least one of the five packages.

---

### Task 1: Environment CRD readiness and security bank scaffolding

**Files:**
- Modify: `internal/environment/apply.go` and its test.
- Modify: `tests/integration/bank_helpers_test.go`, `tests/integration/bank_build_test.go`.
- Create: `tests/integration/bank_security_content_test.go`, `tests/integration/bank_security_test.go`.

**Interfaces:**
- Produces:
  - `var securityIDs = []string{"custom-resource-001", "access-001", "resources-001", "configuration-001", "security-001"}`;
  - `securityCases map[string]map[string]int` (problem → case → expected score), `securityInitialScores map[string]int` and `securityCapabilities map[string][]string` (all nil);
  - `TestSecurityProblems` (per present ID: domain `environment-security`, 5 points, ≥2 seeds, the case table pinned exactly, only `alternative-*` at 5, full status vectors, R1 timeouts, no `${` in private sources, difficulty and type set);
  - `TestSecurityBankPreflight`;
  - `TestSecurityBankSmoke` and `TestSecurityBank` (`validateGroupAfterSmoke(t, loadGroup(t, presentSecurityIDs(t)), false)` until Task 7 removes the present-only guard).

- [ ] **Step 1: RED for CRD readiness.** Add an integration test `TestApplyWaitsForCRDEstablished` in the existing environment integration file: apply a CRD (declared in `Scope.ClusterResources`) in one `Apply` call, then immediately apply a custom resource of that kind in a second `Apply`. Both must succeed and the CR must be readable, over 5 repetitions with fresh names. Add a unit test for the wait helper: Established true → returns; NamesAccepted false or a timeout → error naming the CRD. Run it and record the failure (NoMatch / unknown resource type, or the missing helper).
- [ ] **Step 2: Implement.** In `apply.go`, after a successful apply of an `apiextensions.k8s.io/v1 CustomResourceDefinition`, poll that object until condition `Established=True`. Bound the wait to 30s and use the existing context. On timeout return an error that names the CRD. No other behaviour changes. GREEN: unit tests and `go test -tags=integration ./tests/integration -run '^TestApplyWaitsForCRDEstablished$' -count=1 -timeout=20m -v` (sandbox env).
- [ ] **Step 3: Scaffolding.**
  - `preflightBank` also registers `grader.AddSecurityCheckers(checks, grader.NewSecurityChecker(grader.SecurityConfig{}))`.
  - `smokeBank` uses all criteria for domain `environment-security`, the same as the deployment branch does.
  - Create the two test files with the Produces symbols and an empty `securityCases`.
  - `TestSecurityProblems` passes vacuously; `TestSecurityBankPreflight` with no IDs is a no-op.
- [ ] **Step 4: Verify** `go test ./... -count=1`, `go vet ./...`, `go vet -tags=integration ./...`, `gofmt -l .`.
- [ ] **Step 5: Commit** `feat: wait for applied CRDs to be established` and `test: scaffold environment-security bank`.

### Task 2: `custom-resource-001` (build, easy)

**Files:** Create `problems/custom-resource-001/{problem.yaml,question.en.md,explanation.ko.md,setup/crd.yaml,setup/app.yaml,setup/provided/README.txt,setup/provided/answers/README.txt,solution/*,cases/*}`; Modify `securityCases`.

Package design (pinned):
- Setup applies:
  - CRD `backups.ops.junkloud.dev`, declared in `scope.cluster_resources`. Group `ops.junkloud.dev`, version `v1`, kind `Backup`, namespaced, short name `bk`.
  - Its openAPI schema requires `spec.schedule` (string), `spec.target` (object with `kind` enum `[Deployment, StatefulSet]` and `name`) and `spec.retentionDays` (integer, 1–30).
  - Deployment `ledger` (busybox `sleep 3600`).
- Question:
  - Find the custom resource type the platform provides for backups.
  - Write its full resource name (`<plural>.<group>`) to `answers/backup-crd.txt`.
  - Create `Backup` `nightly` in `${namespace}` with schedule `0 2 * * *`, target Deployment `ledger` and 7 retention days.
  - Do not use a ConfigMap or any other object instead.
- Criteria, 1 point each:
  1. `crd-discovered`: `file` `answers/backup-crd.txt`, `matches` `^\s*backups\.ops\.junkloud\.dev\s*$`.
  2. `backup-exists`: `resource` `ops.junkloud.dev/v1 Backup nightly`.
  3. `backup-schedule`: `field` `spec.schedule` equals `0 2 * * *`; `depends_on` `backup-exists`.
  4. `backup-target`: `field` `spec.target.kind` equals `Deployment` and `spec.target.name` equals `ledger`; `depends_on` `backup-exists`.
  5. `backup-retention`: `field` `spec.retentionDays` equals 7; `depends_on` `backup-exists`.
- Initial: all FAIL → `initial_score` 0.
- Cases (expected score):
  - `configmap-substitute`: ConfigMap `nightly` with the same keys, answer written → 1.
  - `missing-answer`: correct CR, no answer file → 4.
  - `wrong-target`: target StatefulSet `ledger` → 4.
  - `wrong-retention`: 14 → 4.
  - `alternative-extra-fields`: CR with a different key order and an extra optional `spec.description` allowed by the schema; answer with surrounding whitespace → 5.
- Reset: the CRD is owned via scope and removed by cleanup; the CR goes with the namespace.

- [ ] **Step 1: RED** add the cases; `go test ./tests/integration -run 'TestSecurityProblems/custom-resource-001' -count=1 -v` → FAIL (package missing).
- [ ] **Step 2: Author** the package. Static GREEN incl. preflight.
- [ ] **Step 3: Live** `-run '^TestSecurityBank$/^custom-resource-001$' -timeout=40m` on both seeds. Also record that reset removes the CRD (`kubectl get crd` evidence) and that grading without the CRD gives FAIL "not found", not ERROR.
- [ ] **Step 4: Commit** `content: add validated custom resource problem`.

### Task 3: `access-001` (modify, medium)

**Files:** Create `problems/access-001/**`; Modify `securityCases`.

Package design (pinned):
- Setup: ConfigMap `report-config`; Deployment `reporter` running as the namespace `default` ServiceAccount (busybox, `sleep 3600`, `cat` available for the token check).
- Question (the allowed and forbidden actions are listed explicitly):
  - Create ServiceAccount `report-reader`.
  - Grant it, using namespaced RBAC objects only: get, list and watch Pods; get ConfigMap `report-config`.
  - It must NOT be able to: delete Pods, get or list Secrets, create Deployments, list Pods cluster-wide or in `kube-system`, or get any other ConfigMap.
  - The `default` ServiceAccount must not gain any of these rights.
  - Run Deployment `reporter` as `report-reader` with its token mounted.
- Criteria, 1 point each. All `authorization` criteria target `v1 ServiceAccount report-reader` unless noted.
  1. `reporter-ready`: `ready` Deployment `reporter`, replicas 1.
  2. `can-read`: allowed true for get/list/watch `pods`, and for get `configmaps` with name `report-config`.
  3. `least-privilege`: allowed false for delete `pods`; get and list `secrets`; create `deployments` (group `apps`); list `pods` with `cluster_scope: true`; list `pods` in namespace `kube-system`; get `configmaps` with name `other`.
  4. `runs-as-sa`: one check (get `pods`, allowed true) plus `runs_as` Deployment `reporter` with `identity: "token"`; `depends_on` `reporter-ready`.
  5. `default-sa-unchanged`: target SA `default`; allowed false for get and list `pods` and get `configmaps` `report-config`.
- Initial: `reporter-ready` PASS and `default-sa-unchanged` PASS → `initial_score` 2.
- Cases (expected score):
  - `bound-to-default` (Role bound to SA `default`, Deployment unchanged) → 1.
  - `edit-rolebinding` (RoleBinding to ClusterRole `edit`) → 4.
  - `cluster-wide-binding` (ClusterRoleBinding to a ClusterRole with the same rules; declared in `scope.cluster_resources`) → 4.
  - `sa-not-used` (correct RBAC, Deployment keeps `default`) → 4.
  - `alternative-split-roles` (two Roles and two RoleBindings) → 5.
- Note in the explanation: cluster-wide rights created by a learner are not owned and survive reset; this is why the question asks for namespaced RBAC only.

- [ ] **Step 1: RED** add cases; `-run 'TestSecurityProblems/access-001'` FAIL (missing).
- [ ] **Step 2: Author**; static GREEN incl. preflight.
- [ ] **Step 3: Live** `-run '^TestSecurityBank$/^access-001$' -timeout=40m` on both seeds. `edit-rolebinding` must show `least-privilege` FAIL with "must not be allowed" while `can-read` and `runs-as-sa` PASS.
- [ ] **Step 4: Commit** `content: add validated access control problem`.

### Task 4: `resources-001` (troubleshoot, medium)

**Files:** Create `problems/resources-001/**`; Modify `securityCases`.

Package design (pinned):
- Setup:
  - ResourceQuota `team-quota`: hard `requests.cpu: 500m`, `limits.cpu: "1"`, `requests.memory: 256Mi`, `limits.memory: 512Mi`, `pods: "4"`.
  - Deployment `worker`: replicas 2, busybox `sleep 3600`, no resources. Its Pods are rejected by the quota, so the Deployment never becomes Ready.
- Question:
  - Make `worker` run 2 Ready replicas within the quota by setting container `worker` requests `cpu: 100m, memory: 64Mi` and limits `cpu: 200m, memory: 128Mi`.
  - Create LimitRange `defaults` with container `defaultRequest` `cpu: 50m, memory: 32Mi` and `default` `cpu: 100m, memory: 64Mi`.
  - Do not delete or change `team-quota`.
  - State the stability window and timeout.
- Criteria, 1 point each:
  1. `worker-resources`: `field` exact quantities on `spec.template.spec.containers[name=worker].resources.requests/limits`.
  2. `quota-intact`: `field` on ResourceQuota `team-quota`, the 5 hard values (a deleted quota FAILs as not found).
  3. `worker-ready`: `ready`, replicas 2, `depends_on` `worker-resources`.
  4. `quota-used`: `field` `status.used['requests.cpu']` equals `200m`, `depends_on` `worker-ready` and `quota-intact`.
  5. `limitrange-defaults`: `field` on LimitRange `defaults` `spec.limits[type=Container].default*`, `compare: "quantity"`.
- Initial: `quota-intact` PASS → `initial_score` 1. `worker-ready` is FAIL as "prerequisite not satisfied", never ERROR.
- Cases (expected score):
  - `quota-deleted` (`delete-owned` the quota, worker with resources) → 3.
  - `quota-raised` (hard requests.cpu 2) → 3: `quota-intact` FAIL, `quota-used` FAIL as prerequisite.
  - `oversized-requests` (requests cpu 300m) → 2.
  - `limitrange-missing` → 4.
  - `alternative-decimal-quantities` (`cpu: 0.1`, `memory: 64Mi` written as `67108864`, limits `0.2` and `134217728`) → 5.

- [ ] **Step 1: RED** add cases; `-run 'TestSecurityProblems/resources-001'` FAIL (missing).
- [ ] **Step 2: Author**; static GREEN incl. preflight.
- [ ] **Step 3: Live** `-run '^TestSecurityBank$/^resources-001$' -timeout=40m`. On both seeds the initial and `oversized-requests` stages show `worker-ready` FAIL (prerequisite) with no ERROR.
- [ ] **Step 4: Commit** `content: add validated resource quota problem`.

### Task 5: `configuration-001` (troubleshoot, easy) and secret-leak guard

**Files:** Create `problems/configuration-001/**`; Modify `securityCases`, `tests/integration/bank_build_test.go` (`bankProgress` optional forbidden-strings check), `tests/integration/bank_security_test.go`.

**Interfaces:** Produces `bankProgress.forbidden []string`. When it is set, every evaluation it observes is marshalled to JSON and checked for each forbidden string; the CLI-style text (`expected`/`actual`/`reason` of every result) is checked too. A match fails the test without printing the value.

Package design (pinned):
- Setup:
  - ConfigMap `shop-config` `{currency: EUR}`.
  - Secret `shop-secret` `{api-token: jk-canary-7f3c9a2e}` (a fixed canary).
  - Deployment `shop`. Its container `app` reads `$CURRENCY` and `$API_TOKEN`. When `sha256($API_TOKEN)` equals the embedded hash it serves `shop currency=$CURRENCY token=ok`, otherwise `token=bad`. It logs `shop started currency=$CURRENCY`. The script never prints the token.
  - The broken setup sets `CURRENCY` as a literal `USD` and `API_TOKEN` from `secretKeyRef` key `api_token`, which is the wrong key: the container cannot start (CreateContainerConfigError).
  - Service `shop` 80 → 8080.
- Question:
  - Fix `shop` so `CURRENCY` comes from ConfigMap `shop-config` key `currency` and `API_TOKEN` comes from Secret `shop-secret` key `api-token`, both as environment variables (`configMapKeyRef` / `secretKeyRef`).
  - Do not change the ConfigMap, the Secret, the image or the command.
  - The Service must return `shop currency=EUR token=ok`.
- Criteria, 1 point each:
  1. `currency-ref`: `field` `env[name=CURRENCY].valueFrom.configMapKeyRef.name/key`.
  2. `token-ref`: `field` `env[name=API_TOKEN].valueFrom.secretKeyRef.name/key`.
  3. `shop-ready`: `ready`, replicas 1.
  4. `shop-serves`: `http` marker `shop currency=EUR token=ok`, `depends_on` `shop-ready`.
  5. `inputs-unchanged`: `field` on ConfigMap `shop-config` `data.currency` equals `EUR`, and the container image and command equal the setup literals. This criterion carries no dependencies, so the Secret-reading criteria stay independent and the redactor always learns the namespace Secrets.
- Initial: `inputs-unchanged` PASS → 1.
- Cases (expected score):
  - `hardcoded-values` (env literals `EUR` and the token) → 3.
  - `wrong-configmap-key` (key `Currency`; the container cannot start) → 2. `shop-ready` must be FAIL with a learner-fault cause. If the live run shows ERROR, give `shop-ready` a `depends_on` on `currency-ref` and recompute the score; never accept ERROR.
  - `configmap-edited` (ConfigMap value changed to `GBP`, refs correct) → 3.
  - `alternative-optional-false` (refs with `optional: false` and an extra unrelated env var) → 5.
- Leak guard: `TestSecurityBank` sets `forbidden = []string{"jk-canary-7f3c9a2e"}` for this problem. The `hardcoded-values` case must also not leak, even though the literal sits in the Deployment spec.

- [ ] **Step 1: RED.**
  - Add the cases.
  - Add a unit-level test of the leak guard in the content test: a fake evaluation containing the canary must fail the guard, a clean one must pass.
  - `-run 'TestSecurityProblems/configuration-001|TestSecurityLeakGuard'` → FAIL.
- [ ] **Step 2: Implement** the guard and author the package; static GREEN.
- [ ] **Step 3: Live** `-run '^TestSecurityBank$/^configuration-001$' -timeout=40m`: all stages, no leak on any stage including `hardcoded-values`. A leak is a product finding: report it, never weaken the guard.
- [ ] **Step 4: Commit** `content: add validated configuration problem`.

### Task 6: `security-001` (behavioral, hard)

**Files:** Create `problems/security-001/**`; Modify `securityCases`.

Package design (pinned):
- Setup:
  - The bundle Namespace with labels `pod-security.kubernetes.io/enforce: restricted` and `pod-security.kubernetes.io/enforce-version: latest`.
  - Deployment `gateway`: replicas 1, busybox, command `mkdir -p /www && echo gateway-ok > /www/index.html && exec httpd -f -p 8080 -h /www`, an `emptyDir` at `/www`, no securityContext. Restricted PSA rejects its Pods, so it never becomes Ready.
  - Service `gateway` 80 → 8080.
  - Port 8080 is deliberate. A non-root process cannot bind port 80 just because `NET_BIND_SERVICE` is added: Kubernetes sets no ambient capabilities.
- Question:
  - Make `gateway` admissible under the namespace's restricted Pod Security policy without changing the policy, the image or the command: run as non-root UID `10001`, no privilege escalation, drop all capabilities, seccomp `RuntimeDefault`, read-only root filesystem.
  - It must serve `gateway-ok` on Service port 80.
  - State the observation window.
- Criteria, 1 point each:
  1. `policy-enforced`: `field` on `v1 Namespace ${namespace}` `metadata.labels['pod-security.kubernetes.io/enforce']` equals `restricted`.
  2. `workload-unchanged`: `field` on Deployment `gateway` container image and command equal the setup literals.
  3. `declared-context`: `security-context` declared; container `gateway` with `run_as_user` 10001, `run_as_non_root` true, `allow_privilege_escalation` false, `capabilities {drop: [ALL]}`, `seccomp` `RuntimeDefault`, `read_only_root_filesystem` true.
  4. `runtime-context`: `security-context` `behavior` (`uid` 10001, `non_root`, `no_capabilities`, `read_only_root`); `depends_on` `declared-context`, so a rejected Pod is FAIL prerequisite, not ERROR.
  5. `serves-port-80`: `http` Service `gateway` port 80, marker `gateway-ok`; `depends_on` `runtime-context`.
- Initial: `policy-enforced` and `workload-unchanged` PASS → 2.
- Cases (expected score):
  - `policy-weakened` (label set to `privileged`, workload left insecure; Pods run) → 1.
  - `policy-removed-and-hardened` (label removed, workload correct) → 4.
  - `runs-as-root` (runAsUser 0, the rest correct; rejected by PSA) → 2.
  - `alternative-pod-level-context` (runAsNonRoot, runAsUser and seccomp at Pod level; the rest at container level) → 5.
- Live note: the restricted policy must actually reject the setup Pods in the sandbox (record the ReplicaSet FailedCreate event). If PSA or seccomp does not behave as upstream here, report BLOCKED with evidence rather than reshaping the problem.

- [ ] **Step 1: RED** add cases; `-run 'TestSecurityProblems/security-001'` FAIL (missing).
- [ ] **Step 2: Author**; static GREEN.
- [ ] **Step 3: Live** `-run '^TestSecurityBank$/^security-001$' -timeout=40m` on both seeds; record the admission evidence.
- [ ] **Step 4: Commit** `content: add validated security context problem`.

### Task 7: Group totals, coverage, full run, records

**Files:** Modify `curriculum/coverage.yaml`, the content test (group test + competency coverage), `bank_deployment_content_test.go`, `README.md`, `runtime/README.md`, plan records.

- [ ] **Step 1: RED.**
  - `TestSecurityBankContent`: 5 problems, 25 points, all IDs present; remove the present-only guard.
  - `TestSecurityCoverage`: exactly one group `{id: environment-security, domain: environment-security, count: 5, points: 25, problems: securityIDs}`.
  - `TestSecurityCompetencies`: each of the 8 strings is used.
  - The bank must contain at least one problem of each type `build`, `modify`, `troubleshoot` and `behavioral`.
  - `TestDeploymentCoverage`: global 17 / 80 / 17 distinct IDs.
  - Run → FAIL.
- [ ] **Step 2: GREEN.** Append the group to `coverage.yaml`. Update the README bank table, totals and validate loop, and the `runtime/README.md` list.
- [ ] **Step 3: Full live run** once on the frozen tree: smoke, then `-run '^TestSecurityBank$' -count=1 -timeout=180m -v`. Then `go test ./... -count=1`, both vets and gofmt.
- [ ] **Step 4: Records.**
  - Tick the original plan's Task 17 and add an 실행 기록 with SHA, durations, seeds, stage counts, NOT RUN items and the 17/80 partial bank.
  - Fill in this plan's Verified evidence, citing local log paths with the `.superpowers/sdd/2026-11-02-task17-environment-security/` prefix marked local-only.
  - Commit `docs: record validated environment-security milestone`.

## Verified evidence

(Filled in by Task 7.)
