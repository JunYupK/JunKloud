# Task 14 Design & Build Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add and genuinely validate the five Task 14 Design & Build problems (4 points each, 20 total) in existing PR #3.

**Architecture:** First implement only the workspace/image execution needed by image-001, then the bounded PVC persistence checker, then author and validate the five packages. Reuse the existing grader, validation certificates, ownership journals and reset implementation. No Helm, Kustomize or generic patch execution is included.

**Tech Stack:** Existing pinned Go, Kubernetes client-go, Cobra, Docker/kind and declarative YAML packages; Linux development container.

**Spec:** `docs/superpowers/specs/2026-10-14-ckad-training-system-design.md`; original Task 14 in `docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md`.

## Global Constraints

- 공통 Linux 안에서 `ckad`·셸·`kubectl`·채점을 수행한다. 호스트는 M2 / 8GB, Windows / 16GB.
- 초기 검증 기준은 **kind v0.33 / Kubernetes v1.37**. 정확한 patch·digest·Go toolchain·의존성 version을 Task 1에서 실제 공식 아티팩트와 대조해 고정하며 `latest`를 쓰지 않는다.
- Docker Linux VM 메모리 **4GiB**는 최소 사양 검증 목표다. 아직 성능 측정값이 없다.
- 기본 모의시험: **20문제, 120분, 100점**, domain 점수 **20/20/15/25/20**.
- 일반 훈련 세트 **5~20개**, 오답 재도전 **1~20개**. 시간 초과 후에도 수동 제출을 허용한다.
- **문제·요구사항은 영어, 해설·오답 피드백은 한국어**. 모의시험은 전체 제출 후, 훈련은 요청한 문제별로 공개한다.
- `.bashrc`, 편집기 설정, 풀이 파일과 제출 기록을 보존한다. SSH 진입·복귀와 실제 다중 클러스터 전환은 제외한다.
- criterion 결과는 **PASS / FAIL / ERROR**. ERROR가 있으면 확정 총점 없음. 초기·정답·대표 오답·reset 검증을 통과한 문제만 출제한다.
- 일반 수렴 대기 기본 **30초**, 네트워크 개별 timeout **3초**, 최대 **3회**. 긴 관찰이 필요한 문제는 명시적 override를 정의한다.
- API·Docker·파일 삭제에는 소유권 확인을 적용한다. 전역 prune, 호스트 kubeconfig 사용, TLS 검증 해제는 금지한다.
- AI API·runtime AI 출제·웹·계정·클라우드·adaptive training을 추가하지 않는다. 문제는 독창적으로 작성한다.
- 이 계획 승인은 구현 방법 선택과 구분된다. 실행 전 isolation은 using-git-worktrees 지침으로 확인하며 기존 변경을 보존한다.

## Review Focus

1. Existing foreign image tag, rebuild, interrupted build and retagging: retain exact intent/identity and never remove an unrelated/shared image (Task 1).
2. A normal learner build and author validation must reach the same grader; ownership labels are not hidden grading criteria (Tasks 1, 3).
3. File actions, build contexts and archive paths escaping through symlinks/private content: reject before mutation and use one stage workspace (Task 1).
4. EmptyDir, wrong PVC/container/mount, no replacement or lost marker: never certify attachment alone; distinguish platform ERROR (Task 2).
5. Behavioral checker's intentional child-Pod replacement: preserve submitted controller/input fingerprints, bound observation and clean only its own marker (Tasks 2, 3).

## File and interface map

| Task | Produces | Consumer |
| --- | --- | --- |
| 1 | Focused author file/image execution; managed learner image build; journaled image cleanup; Docker24-compatible verified native export | Task 3 image package and real learner/reset test |
| 2 | `volume-persistence` registration, strict arguments, actual write/recreate/read proof and input handling | Task 3 volume package and case validation |
| 3 | Five packages, shared bank integration helper, coverage mapping and CLI documentation | Future Task 15+ append coverage rather than overwrite |

### Task 1: Scoped file/image execution and learner ownership

**Files:** Create `internal/author/{executor,file_actions,image_actions}.go` with focused tests; `internal/session/image_build.go`, `internal/cli/image.go` and tests; `tests/integration/task14_images_test.go`. Modify author validate/compose/workspace, necessary session composition/reset/submission observation, environment image cleanup/inventory, image-run export, runtime documentation. Keep responsibilities split; no unrelated refactor.

**Interfaces:** `author.ActionExecutor` has `Execute(ctx context.Context, id model.Identity, owner environment.Owner, p model.Problem, f model.FrozenProblem, workspace string, actions []model.Action) (model.Inventory,error)`. Full `p` supplies private action sources; frozen public `f` must not regain private content. Preserve `BuildActions` apply/delete-owned compatibility. Runtime composes the executor with the existing Manager, Store, argv Runner and pinned images. Add `session.ImageBuildOptions{ProblemID,ContextDir,Reference string}` and `(*Service).BuildImage(ctx context.Context,sessionID string,opts ImageBuildOptions)(model.OwnedImage,error)`, exposed as `ckad image build --problem Q1 --context . --tag <reference>`.

- [ ] Write focused failing tests for mutated stage workspace versus fresh initial/reset assets, strict preflight before mutation, source/destination symlink escape, unknown action/phase, real image build/export versus wrong/missing artifact, foreign tag refusal, same-owner rebuild, cancellation intent, and learner image ownership/reset.
- [ ] Run affected author/environment/session/cli/grader selections and record the missing-behavior RED; do not count a missing Docker daemon as the expected failure.
- [ ] Implement only `file {source,path}` (private setup/solution/cases source to current workspace) and `image {operation:build,path,reference}` / `{operation:save,reference,path}` plus existing apply/delete-owned. Unsupported Helm/Kustomize/patch remain explicit errors. Stage provided assets BEFORE actions; the same directory reaches actions and grader. Original FrozenProblem.Assets remain unchanged. Reject file/image setup phases before certification unless runtime initial serving is implemented identically; Task 14 packages need these actions only in solution/cases.
- [ ] Preflight the complete list before effects: clean scoped paths, no escaping symlink, strict arguments, offline provided native base images, no remote build inputs or arbitrary host shell/eval. Use bounded argv-based Docker build/save. Private source content must not appear in diagnostics or the initial learning workspace. Invalidate prior certificates by updating the existing version contract when execution semantics change.
- [ ] Managed learner build uses the SAME bounded builder as author actions, takes the environment mutation lock, verifies the running session/problem and current attempt path, journals intent BEFORE Docker effects, attaches environment/session/problem ownership labels and records exact tag/ID. An existing foreign/shared tag is preserved. Same-owner rebuild must not leak earlier owned IDs or lose crash recovery. Never use prune or force removal. Pending build/cleanup must block a new start and have a narrow recorded recovery path.
- [ ] Grade image behavior independently of cleanup labels: a valid direct Docker build can satisfy functional checks; lack of ownership proof may only cause an explicit retained cleanup error. Observe declared image references at grading/reset so an unlabelled or retagged image is never silently treated as cleaned. Document managed build as the way to obtain automatic owned cleanup, with ordinary Docker underlying it. Do not silently adopt a pre-existing foreign tag.
- [ ] Share or narrowly adapt verified native-image export for Docker24/API1.43. Legacy save is allowed ONLY for the explicit unsupported `--platform` API error after checking exact tag/ID and Linux/native architecture; other failures and moved/wrong-architecture refs fail. Existing pinned-image preloading behavior stays intact.
- [ ] Verify affected package tests and `JUNKLOUD_TEST_HOST_COMPAT=no-ipv6 go test -tags=integration ./tests/integration -run '^TestTask14ImageWorkflow$' -count=1 -timeout=20m -v`. Use actual build, archive, grader run and managed CLI learner path, two clean cycles, unchanged foreign sentinel image and archived personal files. Do not certify private author builds alone as evidence of learner reset.
- [ ] Self-review, write report with exact RED/GREEN/live command output and limits, then commit `feat: support scoped image practice workflows`. No push; controller reviews.

### Task 2: Real PVC persistence behavior

**Files:** Create `internal/grader/volume_persistence.go` and focused tests; `tests/integration/task14_volume_test.go`. Modify model/schema criterion enumeration, existing grader registration/composition and input capture/versioning only as needed.

**Interfaces:** New criterion kind `volume-persistence`; Target is one owned `apps/v1 Deployment`; strict Args `{container,path,claim}`. `path` is absolute inside that container's PVC mount, not its root filesystem, EmptyDir, another volume or an escaping subPath. Package timeout is **120s**. Follow the current Checker/ArgsValidator/MutatingChecker conventions; no separate grader engine.

- [ ] Write failing tests: persistent marker after actual owned Pod replacement, EmptyDir/wrong claim/wrong container refusal, no replacement or unreadable marker, unknown args with zero effects, foreign/replaced object UID refusal, cancellation/cleanup, and intentional child replacement does not invalidate an unchanged submitted controller.
- [ ] Run the focused grader/model/schema selections and record RED from the absent behavior.
- [ ] Implement a fresh unpredictable per-check marker, verified controller/ReplicaSet/Pod UID chain and PVC mount proof, bounded write → one owned Pod replacement with UID precondition → new UID ready → marker read. Never rewrite the controller spec, detach another volume, strip finalizers or delete backing data. Remove only the checker's own marker when safe; errors during required cleanup are ERROR. Wrong learner persistence is FAIL; inability to observe/prove safely is ERROR.
- [ ] Record enough evidence to explain old/new Pod UID and persistence result without marker contents/secret leakage. Update input fingerprint handling to ignore only this intentional child observation change, never controller spec, PVC/mount or learner file changes. Bump GraderVersion and certificate invalidation coverage.
- [ ] Run focused regressions and `JUNKLOUD_TEST_HOST_COMPAT=no-ipv6 go test -tags=integration ./tests/integration -run '^TestTask14VolumePersistence$' -count=1 -timeout=15m -v` against real PVC and EmptyDir. After persistence proof, use Task13 owned reset and verify backing removal and initial state, not just Pod recreation.
- [ ] Self-review/report/commit `feat: verify persistence across owned pod replacement`; no push.

### Task 3: Five validated Design & Build packages

**Files:** Create full `problems/<id>/` packages, `curriculum/coverage.yaml`, `tests/integration/bank_build_test.go`, shared `tests/integration/bank_helpers_test.go` if needed; focused static content tests and runtime README examples.

| ID | Points | Required behavior / representative wrong answer |
| --- | ---: | --- |
| image-001 | 4 | Image build, archive and actual run / correct tag with wrong executable output or missing archive |
| batch-001 | 4 | Job and CronJob success / a manual Job substituted for actual schedule |
| daemonset-001 | 4 | DaemonSet node placement / Deployment used instead |
| multicontainer-001 | 4 | Init container, sidecar and real shared temporary-volume data / containers exist but do not share data |
| volume-001 | 4 | Actual PVC persistence / EmptyDir or wrong claim substituted |

**Interfaces:** `loadGroup(t,ids []string)` and `validateGroup(t,problems []model.Problem)` support later bank groups; `sumPoints(problems)` returns declared criterion total. Use existing schema v1/Validator and Tasks1–2 real contracts. Coverage maps these five unique IDs to design-build, count5/points20, without claiming full20question mock coverage.

- [ ] Write failing static/load/coverage tests and real `TestBuildBank`: missing packages rejected, exact IDs/count5, point sum20, each record.Passed true, representative wrong/alternative cases and initial/reset scores agree. Observe RED before content creation.
- [ ] Write original English questions, Korean feedback/explanations, explicit initial states, solutions, multiple representative wrong answers and valid alternative answers. Use pinned multi-architecture provided images and small budgets. All hidden checks map to public requirements; no hidden ownership-label grading. At least two parameter variants and covering seeds per package; keep domains/types/difficulty explicit. Actual CronJob schedule observation has an explicit <=120s window, distinct from template/manual Job checks. Init/sidecar checks require data behavior; volume uses Task2 criterion.
- [ ] Run static content/coverage tests and actual `JUNKLOUD_TEST_HOST_COMPAT=no-ipv6 go test -tags=integration ./tests/integration -run '^TestBuildBank$' -count=1 -timeout=60m -v`; certify every package/covering seed through initial, solved, wrong, alternative and reset with the same real engine. Emit per-problem/seed/stage progress, fail on ERROR, retain logs and exact hashes. Do not rerun the group merely because the waiting controller wants a new update.
- [ ] Run final `go test ./... -count=1` on the frozen Task14 tree; only re-run or broaden checks for a new code change or concrete unresolved failure. Initial native amd64 evidence does not claim an actual M2/4GiB hardware pass.
- [ ] Self-review/report/commit `content: add validated design and build problems`; controller performs one delta/final-integration review reusing completed module reviews. After review, mark original Task14 complete, push this branch and update PR#3 title/body to Tasks12–14. No Task15+ or Helm/Kustomize authoring, no merge.
