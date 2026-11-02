# Task 18 Networking and Default Mock Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Start only after the Task 17 plan (`2026-11-02-task17-environment-security.md`) is complete: mock-01 needs its five problems and its `behavioral` problem.

**Goal:**
- Add `service-001` (6 points), `networkpolicy-001` (8 points) and `ingress-001` (6 points). This is domain `networking`, 20 points, and completes the 20-problem / 100-point bank.
- Add the default mock `exams/mock-01.yaml`: 20 problems, 120 minutes, domain points 20/20/15/25/20, all four problem types.

**Architecture:**
- Content only. Grading uses the existing `NetworkChecker` kinds (`http`, `tcp`, `network-policy`) plus `ready`, `field` and `logs`. Mock loading and serving already exist (`problem/exam.go`, `session/bank.go` `composeMock`, `session/serve.go`).
- No product code change is planned. If a package cannot be expressed with these, report BLOCKED with the gap.

**Tech Stack:** Existing pinned Go, client-go, kind v0.33 / Kubernetes v1.37.0, kindnetd NetworkPolicy enforcement (needs kernel `nft_queue`), cloud-provider-kind Ingress (class `cloud-provider-kind`), provided `docker.io/library/busybox:1.37.0`.

**Spec:** `docs/superpowers/specs/2026-10-14-ckad-training-system-design.md` (§3.1 line 60, §7.2, §8.2); original Task 18 in `docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md`.

## Global Constraints

- 기본 모의시험: **20문제, 120분, 100점**, domain 점수 **20/20/15/25/20**. 기본 `mock-01`은 검토된 20문항 구성을 사용한다. YAML의 검사 배점과 시험에 표시되는 배점은 같아야 한다 (spec §7.2).
- Build, Modify, Troubleshoot, Behavioral 유형을 섞고 기본 시험에 네 유형을 모두 포함한다 (spec §7.2).
- NetworkPolicy의 차단 성공은 timeout 하나만으로 확정하지 않는다. 동일 목적지의 정상 서비스 상태와 허용 경로를 함께 검증하며, 서버가 죽어 모두 차단된 상태는 정답이 아니다 (spec §8.2).
- 검사용 Pod는 사용자 네트워크 문제의 selector·namespace 조건에 맞춰 만든다. 플랫폼의 숨은 정책으로 문제의 조건을 바꾸지 않는다 (spec §8.2).
- Ingress: host/path 라우팅, 애플리케이션 응답, 잘못된 라우팅의 실패를 검사한다 (spec §4.1). `release-001`은 확률적 traffic split 채점을 하지 않는다. 이 plan도 하지 않는다.
- 문제·요구사항은 영어, 해설·오답 피드백은 한국어. 숨은 채점 기준 금지. Every "do not change X" sentence is graded.
- Network criteria:
  - declare `timeout` explicitly (minimum 45s; 60s for an Ingress target); this plan uses `"60s"` for every network criterion (ruling R1);
  - `depends_on` the `ready` criterion of the target workload, because EndpointSlices are read once with no waiting;
  - `network-policy` criteria have at least one allow and one deny, and every deny port has an allow on the same port;
  - cross-namespace sources need `scope.namespaces`.
- Carried rules:
  - only `alternative-*` cases may score full marks;
  - a case that removes fields from a setup-created object uses `delete-owned` and then `apply`;
  - wrong cases are server-accepted states;
  - private `file` sources contain no `${`.
- Capabilities:
  - `networkpolicy-001` declares `["network-policy"]`; `ingress-001` declares `["ingress"]`.
  - Serving refuses a problem whose capability is not PASS. A mock with such a problem is refused as a whole and never silently substituted (`serve.go`).
- **Sandbox limit:** this kernel lacks `nft_queue`, so NetworkPolicy is NOT enforced here.
  - `networkpolicy-001` cannot pass its solved stage here and therefore cannot be certified here.
  - Its live certification is NOT RUN and must happen on an enforcing host (`ckad doctor` network-policy PASS, then `ckad bank validate networkpolicy-001`).
  - Never mark it validated from the sandbox.
- Sandbox env: `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6 JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`. Ingress works here only with `no-ipv6`.

## Review Focus

1. A dead or deny-all backend never satisfies a deny expectation, and an all-allowed namespace never satisfies one either. Both are wrong cases (Task 3).
2. Relabeling Pods to dodge a policy, or changing Pod labels to match a broken Service selector, loses the "workload unchanged" point (Tasks 1, 3).
3. An Ingress that sends every path to one backend, or a wrong backend that answers 200, FAILs the route it breaks (Task 2).
4. `mock-01` composition exactly matches the spec numbers, every problem exists, and no problem appears twice (Task 4).
5. On an environment whose doctor reports network-policy FAIL, `ckad start` (mock-01) is refused with a reason that names `networkpolicy-001`; nothing is substituted (Task 4).

## File map

| Task | Creates | Modifies |
| --- | --- | --- |
| 1 | `tests/integration/bank_network_content_test.go` (linux), `tests/integration/bank_network_test.go` (integration), `problems/service-001/**` | `tests/integration/bank_build_test.go` (`smokeBank` networking branch) |
| 2 | `problems/ingress-001/**` | network case table |
| 3 | `problems/networkpolicy-001/**` | network case table |
| 4 | `exams/mock-01.yaml`, `tests/integration/mock_composition_test.go` (linux), `tests/integration/mock_serving_test.go` (integration) | `curriculum/coverage.yaml`, `TestDeploymentCoverage` (20/100/20), `README.md`, `runtime/README.md`, plan records |

Competency strings (spec §3.1): `NetworkPolicy`, `Service access and troubleshooting`, `Ingress`.

---

### Task 1: Networking bank scaffolding and `service-001` (troubleshoot, medium, 6 points)

**Interfaces:**
- Produces `networkIDs = []string{"service-001", "ingress-001", "networkpolicy-001"}`, `networkCases`, `networkInitialScores` and `networkCapabilities` (service nil, ingress `["ingress"]`, networkpolicy `["network-policy"]`).
- Produces `TestNetworkProblems` (the same per-problem checks as `TestSecurityProblems`, with points per ID 6/6/8), `TestNetworkBankPreflight`, `TestNetworkBankSmoke` and `TestNetworkBank` (present IDs only until Task 4).
- `smokeBank` keeps all criteria for domain `networking`. When `network-policy` is listed in `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES`, it drops `network-policy` criteria and their dependents and logs `NOT RUN: network-policy unenforced`.

Package design (pinned):
- Setup:
  - Deployment `catalog`: 2 replicas, labels `app: catalog, tier: backend`, container port `8080` named `http`, busybox httpd serving `catalog-ok`.
  - Service `catalog`: broken selector `app: catalog-api`, port 80, wrong `targetPort: 9090`, type ClusterIP.
  - Deployment `frontend`: a loop that runs `wget -qO- http://catalog/` every 3s and logs `frontend fetched <body>`.
- Question:
  - Repair Service `catalog` so port 80 (port name `http`) routes to the catalog Pods' port 8080.
  - Make it type `NodePort` with nodePort `30080`.
  - ~~Do not change Deployments `catalog` or `frontend` (labels, image, command).~~ R4/H1: the sentence covers the Pod template of Deployment `catalog` only (labels `app: catalog`, `tier: backend`, image, command), because only that is graded; the question also states that the Service must select by `app: catalog`.
  - `frontend` must log `frontend fetched catalog-ok`.
- Criteria, 1 point each:
  1. `catalog-ready`: `ready`, 2 replicas.
  2. `workloads-unchanged`: `field` catalog template labels, image and command equal setup.
  3. `service-routes`: `http` Service `catalog` port 80, marker `catalog-ok`; `depends_on` 1.
  4. ~~`service-type`~~ R4: `service-shape`: `field` `spec.selector.app` equals `catalog`, `spec.type` equals `NodePort` and `spec.ports[name=http].nodePort` equals 30080.
  5. `frontend-ready`: `ready` Deployment `frontend`.
  6. `frontend-reaches`: `logs` Deployment `frontend`, `matches` `frontend fetched catalog-ok`; `depends_on` 3, 5.
- Initial: 1, 2 and 5 PASS → `initial_score` 3.
- Cases (expected score):
  - `pods-relabeled` (catalog Pod labels changed to `app: catalog-api`, Service port still wrong) → 2.
  - `wrong-target-port` (selector fixed, targetPort 9090) → 4.
  - `clusterip-only` (routes fixed, type ClusterIP) → 5.
  - `empty-service` (selector removed via delete-owned + apply) → ~~4~~ 3 (R4: `service-shape` FAILs too).
  - `pods-labelled-directly` (R4, added: bare Pods labelled `app: catalog-api` serve `catalog-ok`, the Service keeps the broken selector but has the fixed `targetPort`/NodePort) → 5.
  - `alternative-named-target-port` (`targetPort: http`, selector `app: catalog` only) → 6.

- [x] **Step 1: RED.** Add the scaffolding and the `service-001` case table. Run `go test ./tests/integration -run 'TestNetworkProblems|TestNetworkBankPreflight' -count=1 -v` → FAIL (the package is missing).
- [x] **Step 2: Author.** Write the package and the `smokeBank` branch, then confirm static GREEN.
- [x] **Step 3: Live.** Run smoke, then `-run '^TestNetworkBank$/^service-001$' -timeout=40m`. Every stage must match on both seeds with no ERROR.
- [x] **Step 4: Commit** `test: scaffold networking bank` and `content: add validated service troubleshooting problem`.

### Task 2: `ingress-001` (build, medium, 6 points)

Package design (pinned):
- Setup:
  - Deployments `shop-web` (marker `web-ok` at `/` ~~and `/index.html`~~ and, R5, also at `/api/`, so a wrong backend answers 200 and only the body marker tells) and `shop-api` (marker `api-ok` at `/api/` via `/www/api/index.html` and, R6/I3, also at `/`), both busybox httpd on 8080.
  - Services `shop-web` and `shop-api`, each 80 → 8080.
- Question: create Ingress `shop` with class `cloud-provider-kind`.
  - Host `shop.${namespace}.example`: path `/` (Prefix) → `shop-web`, path `/api` (Prefix) → `shop-api`.
  - Host `api.${namespace}.example`: path `/` → `shop-api`.
  - State the 60s limits.
- Criteria, 1 point each:
  1. `web-ready`: `ready`.
  2. `api-ready`: `ready`.
  3. `root-route`: `http` Ingress `shop`, host `shop.${namespace}.example`, path `/`, marker `web-ok`; `depends_on` 1.
  4. `api-path-route`: `http` Ingress, same host, path `/api/`, marker `api-ok`; `depends_on` 2.
  5. `api-host-route`: `http` Ingress, host `api.${namespace}.example`, path ~~`/api/`~~ `/` (R6/I3: the question says host `api` sends `/` to `shop-api`), marker `api-ok`; `depends_on` 2.
  6. ~~`ingress-class`: `field` `spec.ingressClassName` equals `cloud-provider-kind`.~~ R5: `ingress-shape`: `field` class equals `cloud-provider-kind` and a rule exists for each of the hosts `shop.${namespace}.example` and `api.${namespace}.example` (a rule without a host, or a default backend, does not replace a host rule).
- Initial: 1 and 2 PASS → 2.
- Cases (expected score):
  - `all-to-web` (every path → shop-web) → 4.
  - ~~`missing-api-host` → 5.~~ R5: → 4 (the `ingress-shape` criterion also FAILs).
  - `wrong-backend-port` (api backend port 8080 on the Service) → 4.
  - ~~`no-class` (class omitted; the default class still serves) → 5.~~ R5: renamed `alternative-default-class` → 6 (confirmed live: the API server sets the default class on a class-less Ingress at creation, so the stored Ingress equals the solution). Added `wrong-class` (`ingressClassName: nginx`, nothing serves it) → 2.
  - `alternative-port-name` (backends reference Service port by name; extra annotation) → 6.

  Confirm live that `no-class` routes through the default class. If it does not, recompute: it must not be ERROR.
- Before Step 2, check how the `http` checker sets Host and path per criterion. Each route criterion needs its own host and path; if the args cannot express them, report BLOCKED.

- [x] **Step 1: RED** — add the cases and run `-run 'TestNetworkProblems/ingress-001'`; it must FAIL.
- [x] **Step 2: Author** — write the package; static GREEN.
- [x] **Step 3: Live** — run `-run '^TestNetworkBank$/^ingress-001$' -timeout=40m` on both seeds and record the LB address evidence.
- [x] **Step 4: Commit** `content: add validated ingress routing problem`.

### Task 3: `networkpolicy-001` (behavioral, hard, 8 points)

Package design (pinned):
- Namespace `${namespace}` holds four Deployments:
  - `api` (role `api`, Service `api` 80 → 8080, marker `api-ok`);
  - `ledger` (role `ledger`, Service `ledger` 80 → 8080, marker `ledger-ok`);
  - `web` (role `web`);
  - `batch` (role `batch`).

  `web` and `batch` run `sleep`.
- Scope namespace `${namespace}-mon` (declared in `scope.namespaces`): Deployment `monitor` (label `app: monitor`) and Deployment `intruder` (no `app` label).
- Question:
  - Using NetworkPolicies in `${namespace}` only, allow ingress to `api` on port 8080 from `role: web` Pods and from `app: monitor` Pods in `${namespace}-mon`. Deny all other ingress to `api`.
  - Restrict `web` egress to `api` on 8080 plus DNS (UDP/TCP 53 to kube-system), so `web` cannot reach `ledger`.
  - ~~Do not relabel or change any Deployment.~~ Shipped: one "do not change" sentence, about the Pod template of Deployment `batch` only (the one thing `workloads-unchanged` grades).
  - State that blocked means "times out while an allowed client reaches the same Pod".
- Criteria, 1 point each:
  1. `api-ready`: `ready`.
  2. `ledger-ready`: `ready`.
  3. `web-reaches-api`: `http` from `role=web` to Service `api` port 80, marker `api-ok`; `depends_on` 1.
  4. `batch-blocked`: `network-policy` target `api`: allow `role=web`, deny `role=batch`, port 80; `depends_on` 1.
  5. `monitor-allowed`: `network-policy` target `api`: allow `app=monitor` in `${namespace}-mon`, deny `intruder` (`app` absent) in `${namespace}-mon`; `depends_on` 1.
  6. `unlabeled-blocked`: `network-policy` target `api`: allow `role=web`, deny a source with no labels in `${namespace}`; `depends_on` 1.
  7. `web-egress-blocked`: `network-policy` target `ledger`: allow `role=batch`, deny `role=web`; `depends_on` 2.
  8. `workloads-unchanged`: `field` on Deployment `batch`: its template labels, image and command equal setup. Relabeling `batch` is the bypass this point catches. The probe Pods carry the declared labels, so relabeling a Deployment never changes criteria 4–7. A static contract test checks that every case keeps `web` and `api` unchanged.
- Initial (no policies): 1, 2, 3 and 8 PASS. The deny criteria FAIL because the denied source connects → `initial_score` 4.
- Cases (expected score, on an enforcing host):
  - `deny-all` (default-deny ingress + egress for all Pods): 3 FAIL; 4–7 FAIL, because their allows time out → 3.
  - `allow-all-ingress` (ingress to `api` from everywhere, `ingress: [{}]`, plus the correct egress policy) → 5. Criteria 4, 5 and 6 FAIL.
  - `batch-relabeled` (correct policies, batch Pods relabeled `role=web`) → 7. Only criterion 8 FAILs.
  - `missing-egress` (ingress policies only) → 7.
  - `alternative-single-policy` (one NetworkPolicy with two `from` entries plus a separate egress policy) → 8.
  - `and-or-split` (R7/J6, added after the review; the monitor peer is two separate `from` entries, `namespaceSelector` OR `podSelector`, instead of one ANDed peer) → 7. Predicted on an enforcing host: only criterion 5 FAILs (the intruder and the lookalike connect). NOT CERTIFIED like the rest.
- These expected values are predictions until certified on an enforcing host. Record them as such.

- [x] **Step 1: RED** — add the cases and run `-run 'TestNetworkProblems/networkpolicy-001'`; it must FAIL.
- [x] **Step 2: Author** the package. Static GREEN incl. preflight, which validates strict network args, minimum timeouts and the allow/deny pairing.
- [x] **Step 3: Sandbox evidence (not certification).**
  - Run the live smoke, which skips the network-policy criteria and logs NOT RUN.
  - Run one live validation `-run '^TestNetworkBank$/^networkpolicy-001$' -timeout=60m` and record its honest result. Expected: initial matches; solved FAILs on the deny criteria because the cluster does not enforce.
  - Report the run as "NOT CERTIFIED — sandbox lacks NetworkPolicy enforcement". Never edit expected values to match the sandbox.
- [x] **Step 4: Commit** `content: add networkpolicy problem (certification requires an enforcing host)`.

### Task 4: `mock-01`, totals and records

**Files:** see the file map.

**Interfaces:** `exams/mock-01.yaml` follows the `problem/exam.go` schema exactly: `schema_version: 1`, `id: mock-01`, `duration: 120m`, `problems: [20 ids]`.

- [x] **Step 1: RED.** `TestMockComposition` (linux, no cluster) loads `exams/mock-01.yaml` via `problem.LoadExam` and every listed problem via `problem.Load`, then asserts:
  ```go
  testutil.Equal(t, exam.Duration, 120*time.Minute)
  testutil.Equal(t, len(problems), 20)
  testutil.Equal(t, sumPoints(problems), 100)
  testutil.Equal(t, domainPoints(problems), map[string]int{"design-build": 20, "deployment": 20, "observability": 15, "environment-security": 25, "networking": 20})
  testutil.Equal(t, typesPresent(problems), []string{"behavioral", "build", "modify", "troubleshoot"})
  ```
  It also checks that the IDs are distinct and every bank problem appears once. It logs the summed `budget` (CPU and memory) for the record.

  Also add `TestNetworkBankContent` (3 problems, 20 points), `TestNetworkCoverage`, and `TestDeploymentCoverage` 20 / 100 / 20.

  Run them → FAIL (no exam, no group).
- [x] **Step 2: GREEN.**
  - Write `exams/mock-01.yaml`, ordered so that domains interleave and easy problems come first.
  - Append the networking group to `coverage.yaml`.
  - Update the README bank table, totals, validate loop and the mock section (`ckad start` / `ckad start mock-01`), and `runtime/README.md`.
- [x] **Step 3: Serving test (integration, sandbox).** `TestMockServing`:
  - with the sandbox env, `ckad list` (or `session.Service.List`) shows mock-01 under 출제 불가;
  - `ckad start` (default mock) is refused with exit 3, and its reason names `networkpolicy-001` and `network-policy`;
  - no session is left active.

  This proves the all-or-nothing gate. A positive full-mock run is NOT RUN here; it is Task 19 on an enforcing host.
- [x] **Step 4: Full live run** of `TestNetworkBank` for service-001 and ingress-001 on the frozen tree. networkpolicy-001's sandbox result is recorded as not certified. Then run `go test ./... -count=1`, both vets and gofmt.
- [x] **Step 5: Records.**
  - Tick the original plan's Task 18 with an 실행 기록 covering: 20/100 bank, mock-01 composition verified, networkpolicy-001 and positive mock run NOT RUN (enforcing host required), and summed budget vs the 4GiB target.
  - Fill in this plan's evidence section.
  - Commit `docs: record networking bank and mock-01`.

## Package design corrections

The "Package design (pinned)" bullets above are the text the plan was written with. Controller rulings made during implementation superseded some of it; the old text is kept struck through in Tasks 1-3. The shipped packages follow the rulings below.

| Ruling | Package | Correction |
| --- | --- | --- |
| R4 | service-001 | `service-type` became `service-shape` (also `spec.selector.app` equals `catalog`); the question states the label `app: catalog` and that hand-labelling Pods is not the fix; the sentence "do not change" covers only the Pod template of `catalog`; `empty-service` is 3; new case `pods-labelled-directly` = 5; the explanation says EndpointSlices list not-ready endpoints with `ready=false` and no longer uses `kubectl get endpoints`. |
| R5 | ingress-001 | `ingress-class` became `ingress-shape` (class and a rule for each of the two hosts); `no-class` became `alternative-default-class` (6); new case `wrong-class` (2); `missing-api-host` is 4; `shop-web` answers `web-ok` at `/api/` too. |
| R6 | ingress-001 | `api-host-route` probes `/` on `api.${namespace}.example` (as the question says) and `shop-api` answers `api-ok` at `/` and `/api/`; the default-class sentence says the API server sets the class on an Ingress that is *created* without one (an update never does); explanation wording for `<error: ...>` (wrong Service name) vs `<none>` endpoints (wrong port) is cautious, the `curl <ADDRESS>` tip names the workspace container or a Linux Docker host, and a Korean grammar slip is fixed; tautological constant asserts were dropped from the content test. |
| R7 | networkpolicy-001 | Deny-all feedback: in-namespace allowed clients (web, batch) fail DNS resolution, monitor and the controls time out; the gating sentence is `api-ready` gates criteria 3-6, `ledger-ready` gates 7, `workloads-unchanged` is not gated; the question says "a client without the `role` or `app` labels", states that the grader's own platform labels are present, and "the server answers `api-ok`"; new case `and-or-split` (monitor peer written as two `from` entries) predicted 7 with only `monitor-allowed` FAILing (the intruder and the lookalike connect); NOT CERTIFIED like the rest. |
| R7 | all three | The Korean particle after `web-ok`/`api-ok` is `를` (read "오케이"); fixed in ingress-001 (6 places) and networkpolicy-001 (1 place). |

Known residuals (not closable inside the point budget): ingress-001 I1 (editing the Service ports or adding a catch-all backend that answers both markers still scores 6); networkpolicy-001 J1 (an ingress policy on `ledger` also earns criterion 7; "policies in `${namespace}` only" is not graded). Doctor caveat: its network-policy check proves ingress deny only, so on an enforcing host watch criterion 7 and deny-all first.

## Verified evidence

Logs are in the local, uncommitted `.superpowers/sdd/2026-11-02-task18-networking-mock/task-4-logs/` (local-only); key raw lines are quoted. Sandbox env for every live run: `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6 JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy`.

### Static
- RED (`red-static.log`): `TestDeploymentCoverage` got 17 want 20, `TestNetworkCoverage` got 0 want 1, `TestMockComposition` `read exam: open ../../exams/mock-01.yaml: no such file or directory`. GREEN (`green-static-1.log`): `TestMockComposition`, `TestNetworkBankContent`, `TestNetworkCoverage`, `TestDeploymentCoverage` PASS.
- `TestMockComposition`: duration 120m, 20 problems, 100 points, domain points design-build 20 / deployment 20 / observability 15 / environment-security 25 / networking 20, types behavioral/build/modify/troubleshoot, IDs distinct and equal to the 20 bank directories, no two neighbours share a domain, the two easy problems are in the first three positions, the two hard ones in the last two. Order: configuration-001, service-001, custom-resource-001, batch-001, probes-001, rollout-001, ingress-001, multicontainer-001, diagnostics-001, release-001, access-001, daemonset-001, api-migration-001, kustomize-001, volume-001, resources-001, helm-001, image-001, security-001, networkpolicy-001.
- Summed declared budget of mock-01: `cpu 8950m, memory 3744 MiB (3.66 GiB) over 20 problems` (largest: networkpolicy-001 700m/256Mi, resources-001 500m/256Mi). Below the 4 GiB target as a sum of declared budgets; not a measured peak.
- Final `go test ./... -count=1` (14 packages ok), `go vet ./...`, `go vet -tags=integration ./...`, `gofmt -l .` all clean (`verify-final.log`).

### Serving (`serving-1.log`, tree before the records commit, PASS 240.24 s)
- Doctor: `DOCTOR network-policy FAIL: a pod not admitted by the NetworkPolicy reached the server: NetworkPolicy is not enforced`; dns, service, ingress, storage, metrics, image-build PASS.
- A stand-in certificate (current problem hash, grader version, environment hash, covering seeds) was stored in the test's temporary store for each bank problem. The stand-ins are not validation evidence; they only take the certificate check (which runs before the capability check) out of the way, because networkpolicy-001 cannot be certified here.
- Stage A (19 stand-ins): `REFUSAL (uncertified): session: a problem has no current validation certificate: networkpolicy-001 (no passing validation for this exact package ...)`.
- Stage B (20 stand-ins): `REFUSAL (capability): session: a required environment capability is unavailable: networkpolicy-001 (needs network-policy (FAIL: a pod not admitted by the NetworkPolicy reached the server: NetworkPolicy is not enforced))`; the error names no other problem and is not `ErrNotCertified`.
- CLI: `ckad start` and `ckad start mock-01` exit 3, stdout empty, stderr `필요한 환경 capability가 준비되지 않았습니다. ckad doctor로 확인하세요.` (the CLI shows only this generic text); `ckad list` exit 0, no `모의시험 mock-01` line, exactly one `출제 불가: networkpolicy-001` line, 19 problems offered. Active sessions are 0 after every step.
- A positive full-mock start is NOT RUN (needs an enforcing host; Task 19).

### Full run (validation of record)
- Frozen tree `0a7d12c5a318b448706dfce3893fe9552f7b0328` (`test: prove the all-or-nothing mock gate and update the bank docs`); later commits change documentation only. Smoke `-run '^TestNetworkBankSmoke$' -timeout=60m`: `--- PASS: TestNetworkBankSmoke (253.07s)` (service-001 57.23 s, ingress-001 65.25 s, networkpolicy-001 60.00 s), with `SMOKE networkpolicy-001 NOT RUN: network-policy unenforced; skipped criteria [batch-blocked monitor-allowed unlabeled-blocked web-egress-blocked]`.
- Full `go test -tags=integration ./tests/integration -run '^TestNetworkBank$' -count=1 -timeout=150m -v`, 2026-11-02T18:45:36Z to 19:46:43Z, exit 1: `--- FAIL: TestNetworkBank (3665.06s)`, `--- PASS: TestNetworkBank/service-001 (1032.69s)`, `--- PASS: TestNetworkBank/ingress-001 (1284.67s)`, `--- FAIL: TestNetworkBank/networkpolicy-001 (1277.49s)`. The parent FAIL is the expected consequence of networkpolicy-001.
- Hashes and seeds: service-001 `sha256:53d9db3a9314d140e922f0783cd5aaa1b005bc9e1ecb7bf94af56263147ae001` seeds [1 4]; ingress-001 `sha256:3ec142a8066005379318c4704dcfb63abec08f7d74ebf7821adb371e8eacb7bd` seeds [1 2]; networkpolicy-001 `sha256:d11f9c2ef1ad6fc0dba2558f81276bf2f0e56913bbee4850b6f1417b4d784ac3` seeds [1 4]. 54 stages (18 + 18 + 18), every one `complete=true error=<nil>`, 0 ERROR results, no reseeding, no timeout change, no earlier failed full run on this tree; no cluster or container left.
- service-001 (both seeds identical, all expected statuses matched): initial 3, solved 6, pods-relabeled 2, wrong-target-port 4, clusterip-only 5, empty-service 3, pods-labelled-directly 5, alternative-named-target-port 6, reset 3. CERTIFIED (sandbox).
- ingress-001 (both seeds identical, all matched): initial 2, solved 6, all-to-web 4, missing-api-host 4, wrong-backend-port 4, wrong-class 2, alternative-default-class 6, alternative-port-name 6, reset 2. Live observations: `api-path-route`/`api-host-route` in all-to-web `answered HTTP 200 but the body lacks the expected application marker: a different backend answered`; missing-api-host `api-host-route ... answered HTTP 404, expected 200`. CERTIFIED (sandbox), including the R6 `/` probe.
- networkpolicy-001 (both seeds identical): NOT CERTIFIED. Got (want): initial 4 (4), solved 4 (8), deny-all 4 (3), allow-all-ingress 4 (5), batch-relabeled 3 (7), missing-egress 4 (7), and-or-split 4 (7), alternative-single-policy 4 (8), reset 4 (4). Raw failure lines: `seed 1: solved: criterion "batch-blocked": got FAIL, want PASS (1 of 2 expectations not met: batch-denied: pod 10.244.0.168: connected although this source must be denied)`, the same for `monitor-allowed` (intruder-denied and monitor-lookalike-denied), `unlabeled-blocked` and `web-egress-blocked`; `seed 1: case "deny-all": criterion "web-reaches-api": got PASS, want FAIL`; `seed 1: case "batch-relabeled": criterion "workloads-unchanged"` FAILs as predicted. Every mismatch is a deny expectation that connects on a cluster that does not enforce NetworkPolicy; no expected value was edited. The `and-or-split` trace (enforcing host: `namespaceSelector` entry admits every Pod of `${namespace}-mon`, the `podSelector` entry admits the `app=monitor` lookalike in `${namespace}`, so only criterion 5 FAILs and the score is 7) is unverified.
- Not verified: networkpolicy-001 on an enforcing host, any positive `ckad start` of mock-01, native M2/4GiB/Windows.
