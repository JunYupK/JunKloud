# JunKloud CKAD Training System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 공통 Linux 환경에서 검증된 20문항 CKAD 모의시험과 문제별 훈련·결정적 채점·오답 재도전을 제공한다.

**Architecture:** 단일 Go CLI가 전용 kind 클러스터의 수명 주기와 세션을 관리한다. 선언형 문제를 동일한 grader로 작성 검증과 실제 채점에 사용하고, 결과·복구 기록은 SQLite에 저장한다. Linux 작업 컨테이너와 kind 노드는 호스트 Docker 엔진을 공유하며, 웹 서비스·별도 애플리케이션 서버를 만들지 않는다.

**Tech Stack:** Go, Cobra, client-go, Kubernetes API machinery, `sigs.k8s.io/yaml`, `github.com/santhosh-tekuri/jsonschema/v6`, `database/sql` + `modernc.org/sqlite`, Go 표준 testing, Docker Compose, kind, cloud-provider-kind, Helm.

**Spec:** [승인된 설계](../specs/2026-10-14-ckad-training-system-design.md). 실행자는 이 계획과 spec을 모두 읽는다.

**Status:** 2026-10-14 사용자 승인 완료. 실행 방식은 **Subagent-driven**으로 선택했다. 아래 체크박스는 Task 1부터 순서대로 수행하며, 체크는 해당 검증을 실제로 통과한 뒤에만 표시한다.

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

1. **복수 터미널·kill·호스트 sleep:** 중복 start/reset과 진행 상태 손상을 막고 시간·중단 기록을 보존한다. Task 3, 10, 11, 13의 프로세스 테스트로 확인.
2. **한글·공백 경로와 symlink·아키텍처:** 호스트 경로를 Linux 경로로 오인하지 않고 파일 이탈·에뮬레이션 의존을 막는다. Task 1, 2, 8, 19에서 확인.
3. **서버 다운·DNS 실패로 생기는 가짜 NetworkPolicy 정답:** 대조 검사 없이 차단 성공을 주지 않는다. Task 7, 18에서 확인.
4. **채점 중 변경·retry 후 상태 차이·부분 저장:** 현재 재관찰을 과거 snapshot으로 속이지 않고, 불완전 점수를 확정하지 않는다. Task 6, 8, 11에서 확인.
5. **controller가 만든 리소스·PV 데이터·동명 다른 클러스터:** reset은 완결적으로 수행하되 타 환경과 사용자 답안을 지우지 않는다. Task 4, 13, 19에서 확인.

## 1. 현재 근거와 실행 경계

기준 커밋은 `cedafec`이며 기존 제품 코드는 없다. 현재 README와 승인된 spec만 있다. 이 계획 작성에서는 Markdown 문서만 추가·수정한다.

공식 curriculum v1.37의 원본을 2026-10-14에 직접 읽고 2페이지를 렌더링했다. SHA-256은 `7736d2f0291239d62e9fc618304413ee9b6e53fc4580aac70bf4328203c1c210`이며 배점과 역량 매핑이 spec과 일치한다. PDF 확인 보류 항목은 해소됐다. 실행 환경·성능 검증은 여전히 미수행이다.

본 시스템의 환경·schema·grader·session은 같은 실행 계약을 공유하므로 한 계획 안에서 순서대로 구현한다. 문제은행 확대는 공통 엔진이 검증된 다음 진행한다. 라이브러리는 위 Tech Stack으로 선택하되 release lock은 호환성 확인 후 작성한다. 참고: [Cobra](https://github.com/spf13/cobra), [client-go](https://github.com/kubernetes/client-go), [JSON Schema](https://github.com/santhosh-tekuri/jsonschema), [SQLite driver](https://pkg.go.dev/modernc.org/sqlite).

### 실행·테스트 규칙

- 아래 상대 경로는 모두 저장소 루트 기준이다. 코드와 테스트 파일은 해당 Task를 실행할 때 생성한다.
- Task 1 이후 Go·Make 명령은 `/src`를 저장소 루트로 갖는 **Linux dev 컨테이너**에서 실행한다. 호스트 PowerShell에 Go나 GNU 도구가 있다고 가정하지 않는다.
- `make test` = `go test ./...`; `make test-race` = 지원되는 Linux dev 환경의 `go test -race ./...`; `make test-integration` = `go test -tags=integration ./tests/integration/... -count=1`; `make test-e2e` = `go test -tags=e2e ./tests/e2e/... -count=1`.
- 실제 클러스터 테스트는 생성한 JunKloud 테스트 environment만 사용한다. 실행 대상·아키텍처·4GiB 설정을 결과에 기록한다. 실제 장비가 없으면 해당 검증을 통과로 표시하지 않는다.
- 테스트 코드 블록은 지정된 테스트 함수에 넣을 핵심 assertion이다. fixture와 fake는 각 Task의 Test 파일에서 정의하며 제품에 숨은 test 전용 분기를 넣지 않는다.
- 처음 실패는 누락된 동작 때문임을 확인한다. 환경 미설치·네트워크 차단을 제품의 예상 실패로 세지 않는다. 각 Task는 최소 구현 → 해당 검증 통과 → 정확한 파일만 stage → 제시된 메시지로 commit한다. 자동 push·merge는 하지 않는다.

## 2. 파일 구조와 공통 계약

| 경로 | 책임 / 소유 Task |
| --- | --- |
| `Dockerfile`, `compose.yaml`, `Makefile`, `go.mod`, `go.sum`, `.gitignore`, `.dockerignore` | dev/workspace 환경과 검증 진입점 / 1, 12, 19 |
| `runtime/versions.lock.yaml`, `runtime/bashrc.defaults`, `runtime/entrypoint.sh` | 고정 도구·이미지 version, 개인 설정 최초 초기화 / 1 |
| `cmd/ckad/main.go`, `internal/cli/*.go` | 명령 라우팅·입출력·의존성 조립 / 12 |
| `internal/model/{problem,session,result,environment}.go` | 아래 공통 값 타입 / 2 |
| `schemas/problem-v1.schema.json`, `internal/problem/{load,render,select,paths}.go` | 선언형 schema·동결·안전 경로 / 2 |
| `internal/store/{sqlite,migrate,session,submission,inventory,validation}.go`, `internal/store/migrations/001_initial.sql` | SQLite 기록·트랜잭션 / 3 |
| `internal/lock/lock_linux.go`, `internal/executil/runner.go` | 환경 잠금 / 3, 안전한 외부 명령 / 4 |
| `internal/environment/{identity,kind,kubeconfig,apply,doctor,capabilities,cleanup}.go` | 전용 환경·플랫폼 기능·초기화 / 4, 5, 13 |
| `internal/grader/{engine,resource,ready,network,artifacts,security,evidence}.go` | 실제 상태·동작 판정 / 6, 7, 8 |
| `internal/author/{actions,validate,certificate}.go` | 문제 검증 / 9 |
| `internal/session/{prepare,start,submit,retry,review,reset,recover}.go` | 세션·공개·복구 / 10, 11, 13 |
| `problems/<id>/`, `exams/mock-01.yaml`, `curriculum/coverage.yaml` | 검증된 문제 package·시험·역량 연결 / 14–18 |
| `tests/{runtime,integration,e2e}/`, `.github/workflows/ci.yaml`, `docs/{usage,authoring,validation}/` | 실제 환경 검증·운영 안내 / 각 Task, 19 |

Task 2에서 `internal/model`에 아래 타입을 정의한다. 패키지 간에는 이 값 타입을 공유하고 SDK·DB 타입을 바깥 계약으로 노출하지 않는다. 고유 ID는 문자열, 시각은 UTC `time.Time`, 배점은 `int`, JSON/YAML property 이름은 snake_case로 통일한다.

| 타입 | 필수 필드 / 의미 |
| --- | --- |
| `ObjectRef` | `APIVersion, Kind, Namespace, Name string` |
| `Criterion` | `ID, Kind string; Target ObjectRef; Points int; DependsOn []string; Timeout time.Duration; Args map[string]any`. Args는 Kind별 schema로 제한 |
| `Action` | `Kind string; Args map[string]any`. apply/delete-owned/helm/image/file 및 사례의 의미 기반 patch만 허용, shell/eval 없음 |
| `Problem` | `SchemaVersion int; ID, Version, Title, Domain, Difficulty, Type string; Tags, Competencies, Capabilities []string; Parameters map[string][]any; Criteria []Criterion; Setup, Solution []Action; InitialExpected map[string]string; InitialScore int; Cases []Case` |
| `Case` | `ID string; Actions []Action; Expected map[string]string; ExpectedScore int` |
| `FrozenProblem` | `Definition Problem; Hash, Question string; Parameters map[string]any; Assets map[string][]byte; Context, Namespace string`. private package 내용은 별도 hash 경로에 저장 |
| `Identity` | `ID, ClusterName, Context, Endpoint, CAHash, MarkerUID string; ContainerIDs []string; Versions map[string]string` |
| `OwnedResource` / `Inventory` | `Ref ObjectRef; UID, OwnerID string` / `EnvironmentID, SessionID, ProblemID string; Resources []OwnedResource; DockerIDs, ArtifactPaths []string` |
| `Operation` | `ID, EnvironmentID, SessionID, Kind, Phase string; Planned Inventory; CreatedAt time.Time`. 실제 생성 전 intent 저장 |
| `Evidence` | `Expected, Actual any; ObservedAt time.Time; UID string; Generation int64; InputHash string; Duration time.Duration`. Secret 원문 제외 |
| `CheckResult` / `Evaluation` | `CriterionID, Status, Reason string; Points int; Evidence Evidence` / `Results []CheckResult; Score *int; MaxScore int; Complete bool`. ERROR이면 Score=nil |
| `SessionRecord` | `ID, EnvironmentID, Mode, State, ParentID string; Seed uint64; Problems []FrozenProblem; StartedAt *time.Time; DurationLimit time.Duration; Marks map[string]bool` |
| `Submission` | `ID, SessionID, Kind, ParentSubmissionID string; SubmittedAt time.Time; InputHashes map[string]string` |
| `GradingRun` | `ID, SubmissionID string; StartedAt, FinishedAt time.Time; Evaluation Evaluation` |
| `ValidationRecord` | `ProblemHash, GraderVersion, EnvironmentHash string; Seeds []uint64; Passed bool; Cases map[string]Evaluation` |

배점·공개·상태 전이 등의 판단은 각 담당 Task에서 구현한다. 여기에 범용 service layer나 event bus를 추가하지 않는다.

## 3. 작업 순서

| 단계 | Task | 완료 시 확인할 수 있는 것 |
| --- | --- | --- |
| A. 공통 실행·안전한 기반 | 1–5 | 양쪽 아키텍처용 Linux 환경, 전용 cluster, capability 검증 |
| B. 채점·문제 검증 | 6–9 | 실제 상태를 판정하고 정답·오답을 가려내는 공통 engine |
| C. 사용 가능한 전체 흐름 | 10–13 | 소수의 검증용 문제로 start→grade→review→reset 실행 |
| D. 실전 문제·출시 검증 | 14–19 | 20문항·100점 모의시험, 전 영역 coverage, 실기기 반복 검증 |

이하 본문과 사용자용 문서는 한국어로 기술한다. 단계 C의 검증용 문제는 제품의 20문항으로 세지 않는다.

### Task 1: 재현 가능한 Linux 개발·작업 환경

**Files:** Create 루트 환경 파일, `runtime/`의 3개 파일, `tests/runtime/workspace_test.sh`, `internal/testutil/assert.go`.

**Interfaces:** Produces `versions.lock.yaml`의 `schema_version=1`, architecture별 checksum/digest와 도구 version; `testutil.Equal(t testing.TB, got, want any)`, `NoError(t testing.TB, err error)`, `ErrorIs(t testing.TB, err, want error)`.

- [ ] **실패 테스트 작성:** `workspace_test.sh`에서 재접속 후 사용자가 추가한 alias·파일이 유지되고, Linux·네이티브 CPU로 실행되며, kind/kubectl/Helm의 실제 version이 lock과 같은지 확인한다. 임시 volume만 사용한다.
  ```bash
  test "$(uname -s)" = Linux
  test "$(cat /home/learner/persist-check)" = keep
  bash -ic 'alias user_check' | grep -q keep
  ```
- [ ] **실패 확인:** 환경 파일을 추가한 후 동작을 구현하기 전 `make test-runtime`을 실행해 초기화·영속성 부재를 확인한다. Docker 자체가 없으면 이 Task의 실행 전제부터 해결한다.
- [ ] **최소 구현:** Dockerfile에 `dev`와 `workspace` target, Compose에 named volume·workspace/dev 서비스를 정의한다. dev만 Go compiler와 race test용 빌드 도구를 포함한다. `entrypoint.sh`는 기존 `.bashrc`를 덮어쓰지 않는다. module은 `github.com/JunYupK/JunKloud`로 정한다. 위 Tech Stack의 실제 호환 release를 조회·고정하고 Go module checksum과 binary checksum을 검증한다. kind는 v0.33.0, Kubernetes는 v1.37.0 아티팩트를 우선 검증한다.
- [ ] **통과 확인:** `make test-runtime` PASS; Linux ARM64·AMD64 이미지 build 확인. 한글·공백이 있는 checkout 경로에서도 같은 결과. Windows의 Linux backend와 Docker socket 전달을 확인한다. 양쪽 이미지를 build한 것과 실제 양쪽 장비 테스트는 구분한다.
- [ ] **커밋:** 이 Task의 파일만 stage. `build: add reproducible Linux workspace`.

### Task 2: 문제 schema·동결·선택과 파일 경계

**Files:** Create `internal/model/` 4개 파일, schema, `internal/problem/` 4개 파일 및 각각의 `_test.go`, `internal/problem/testdata/valid-service/`·`invalid/`.

**Interfaces:** Produces `problem.Load(dir string) (model.Problem,error)`, `Freeze(p model.Problem, seed uint64) (model.FrozenProblem,error)`, `Select(bank []model.Problem, opts SelectOptions) ([]model.Problem,error)`, `ResolveAsset(root,path string) (string,error)`. `SelectOptions{Mode,Domain,Difficulty,Tag string; Count int; Seed uint64}`.

- [x] **실패 테스트 작성:** `TestFreezeStable`, `TestInvalidDefinition`, `TestAssetEscape`, `TestSelectInsufficient`. 정상 fixture ID는 `service-test`, primary domain은 `networking`. 중복 YAML key·알 수 없는 kind·순환 DependsOn·잘못된 배점·범위 밖 포트·중복 ID를 거부한다.
  ```go
  a, err := problem.Freeze(p, 38129); testutil.NoError(t, err)
  b, err := problem.Freeze(p, 38129); testutil.NoError(t, err)
  testutil.Equal(t, a, b)
  _, err = problem.ResolveAsset(root, "../solution/answer.yaml")
  testutil.ErrorIs(t, err, problem.ErrAssetEscape)
  ```
- [x] **실패 확인:** `go test ./internal/problem -run 'TestFreeze|TestInvalid|TestAsset|TestSelect' -v`에서 미구현 동작 실패 확인.
- [x] **최소 구현:** strict YAML→JSON 변환과 JSON Schema 2020-12 validation 후 타입 변환. 수량·시간 문자열의 parsing을 명시한다. 이름 등 scalar parameter만 구조적으로 치환하고 누락 변수를 거부한다. hash는 정규화한 정의와 package 파일을 정렬해 계산한다. symlink 실제 경로와 상위 이탈을 검사한다. 난수 알고리즘 version은 `seed-v1`로 기록하며 정렬된 후보와 고정 PRNG 규칙을 golden test에 고정한다.
- [x] **통과 확인:** 위 테스트와 `go test ./internal/model ./internal/problem` PASS. 다른 seed의 유효값, 한글·공백 경로, 중복 선택 방지, 5~20 / retry 1~20 경계를 확인한다.
- [x] **커밋:** `feat: validate and freeze declarative problems`.

### Task 3: SQLite 기록·원자적 결과·환경 잠금

**Files:** Create Store·migration·lock 파일과 `internal/store/sqlite_test.go`, `internal/lock/lock_test.go`.

**Interfaces:** Produces `store.Open(path string) (*Store,error)`, `Close() error`; Store methods `CreateSession(ctx,SessionRecord) error`, `LoadSession(ctx,id string) (SessionRecord,error)`, `Transition(ctx,id,from,to string) error`, `PutOperation(ctx,Operation) error`, `PutInventory(ctx,Inventory) error`, `CreateSubmission(ctx,Submission) error`, `SaveRun(ctx,GradingRun) error`, `SaveValidation(ctx,ValidationRecord) error`, `RecordReview(ctx,sessionID,problemID string,at time.Time) error`. `lock.Acquire(path string) (release func() error,err error)`.

- [x] **실패 테스트 작성:** `TestSaveRunAtomic`, `TestTransitionCompareAndSwap`, `TestCorruptDBPreserved`, `TestLockAcrossProcesses`. 별도 프로세스의 경합, 도중 rollback, 같은 제출의 변경 거부를 검증한다.
  ```go
  err := s.Transition(ctx, "s1", "RUNNING", "GRADING")
  testutil.NoError(t, err)
  err = s.Transition(ctx, "s1", "RUNNING", "GRADING")
  testutil.ErrorIs(t, err, store.ErrStateConflict)
  ```
- [x] **실패 확인:** `go test ./internal/store ./internal/lock -v`에서 상태 충돌·중복 잠금 관련 실패 확인.
- [x] **최소 구현:** `database/sql`+modernc driver, foreign keys, bounded busy timeout, migration version, 명시적 트랜잭션. 제출·grading run은 append-only이며 update로 점수를 덮어쓰지 않는다. Linux file lock으로 프로세스 종료 시 잠금이 해제되게 한다. DB 손상 시 원본 보존. API 작업과 DB transaction을 묶었다고 가정하지 않는다.
- [x] **통과 확인:** 동일 명령 PASS 및 `go test -race ./internal/store ./internal/lock`. kill된 프로세스의 lock 회수와 다른 조회 프로세스의 동작 확인.
- [x] **커밋:** `feat: persist sessions and recoverable operations`.

### Task 4: 전용 클러스터 식별·명령 실행·초기 적용

**Files:** Create `internal/executil/runner.go`, environment의 identity/kind/kubeconfig/apply 파일 및 unit tests, `tests/integration/environment_test.go`.

**Interfaces:** Consumes Task 2 Identity/Inventory/Operation과 Task 3 Store. Produces `executil.Run(ctx context.Context, exe string,args []string,stdin io.Reader) (Result,error)` with `Result{Stdout,Stderr []byte; ExitCode int}`; `environment.New(stateDir string,s *store.Store) *Manager`; methods `Ensure(ctx) (Identity,error)`, `Check(ctx,id Identity) error`, `Apply(ctx,id Identity,owner Inventory,actions []Action) (Inventory,error)`.

- [x] **실패 테스트 작성:** `TestRejectForeignIdentity`, `TestIgnoreAmbientKubeconfig`, `TestRecoverCreateIntent`, `TestExecCancellation`. 이름이 같아도 CA/marker UID/container ID가 다른 환경을 거부한다.
  ```go
  err := manager.Check(ctx, foreignIdentity)
  testutil.ErrorIs(t, err, environment.ErrIdentityMismatch)
  testutil.Equal(t, foreignClusterMutationCount, 0)
  ```
- [x] **실패 확인:** `go test ./internal/executil ./internal/environment -v`에서 소유권 거부 동작 실패 확인.
- [x] **최소 구현:** create intent를 먼저 저장하고 환경 ID가 포함된 namespace·label·Docker metadata를 적용한다. 내부 endpoint와 정당한 CA 검증을 갖춘 별도 kubeconfig를 만든다. host KUBECONFIG와 current-context를 읽지 않는다. 외부 명령은 인자 배열·bounded output·context 취소로 실행한다. 작업 실패를 자동 글로벌 삭제로 복구하지 않는다.
- [x] **통과 확인:** unit PASS; `go test -tags=integration ./tests/integration -run TestEnvironment -count=1 -v` PASS. create 직후 중단을 주입해 기록된 intent로 소유 대상을 재확인한다. 다른 cluster sentinel이 그대로임을 확인한다.
- [x] **커밋:** `feat: manage an explicitly owned kind environment`.

### Task 5: 플랫폼 capability와 doctor

**Files:** Create environment doctor/capabilities 파일, `runtime/platform/`의 고정 manifest, `tests/integration/capabilities_test.go`.

**Interfaces:** Consumes Manager.Check/Apply. Produces `Manager.Doctor(ctx,id Identity) (DoctorReport,error)`; `DoctorReport{EnvironmentID string; Checks []CapabilityCheck; Versions map[string]string}`, `CapabilityCheck{Name,Status,Reason string}`. capability 이름은 `dns,service,network-policy,ingress,storage,metrics,image-build`.

- [x] **실패 테스트 작성:** `TestPlatformCapabilities`, `TestDoctorDoesNotReadAnswers`. DNS와 Service 양성 연결, NetworkPolicy 음성·양성, Ingress host/path, PVC 쓰기·읽기, metrics API, image build를 실제로 검사한다.
  ```go
  report, err := manager.Doctor(ctx, id); testutil.NoError(t, err)
  for _, check := range report.Checks { testutil.Equal(t, check.Status, "PASS") }
  testutil.Equal(t, answerNamespaceReadCount, 0)
  ```
- [x] **실패 확인:** 부가 기능 미설치 상태에서 integration 명령이 해당 capability 실패를 보고하는지 확인한다.
- [x] **최소 구현:** cloud-provider-kind의 독립 프로세스 수명 주기·재시작·종료 처리, 필요한 metrics/storage 구성과 사전 이미지 준비를 고정한다. doctor는 전용 platform probe만 사용하고 시험 namespace를 읽지 않는다. 기능 실패를 capability 비활성 상태로 보고해 해당 문제 출제를 막는다.
- [ ] **통과 확인:** `go test -tags=integration ./tests/integration -run 'TestPlatform|TestDoctor' -count=1 -v` PASS. M2 4GiB에서 기능 구성 자체가 성립하는지 조기 확인하고 불가하면 나머지 제품 구현 전 설계로 되돌린다.
- [x] **커밋:** `feat: verify required Kubernetes capabilities`.

### Task 6: 공통 채점 엔진과 설정·상태 판정

**Files:** Create grader engine/resource/ready/evidence 파일, 각각 `_test.go`, `tests/integration/resource_grading_test.go`.

**Interfaces:** Produces `grader.Checker` with `Check(context.Context,Target,model.Criterion) (model.CheckResult,error)`; `Target{Identity model.Identity; Problem model.FrozenProblem; Workspace string}`; `NewEngine(checkers map[string]Checker) *Engine`; `Engine.Evaluate(ctx,Target) (model.Evaluation,error)`. unit tests용 fake Checker는 test 파일에서 정의한다.

- [x] **실패 테스트 작성:** `TestPartialScore`, `TestErrorKeepsScorePending`, `TestQuantityEquivalence`, `TestObservedGeneration`, `TestEvidenceRedaction`. 2+2+2+4 배점 fixture에서 PASS 2개(2·4), FAIL 2개는 6/10이고 ERROR가 하나 생기면 Score=nil이다.
  ```go
  testutil.Equal(t, *graded.Score, 6)
  testutil.Equal(t, pending.Score, (*int)(nil))
  testutil.Equal(t, pending.Complete, false)
  testutil.Equal(t, strings.Contains(encodedEvidence, "secret-canary-value"), false)
  ```
- [x] **실패 확인:** `go test ./internal/grader -run 'TestPartial|TestError|TestQuantity|TestObserved|TestEvidence' -v` 실패 확인.
- [x] **최소 구현:** criterion DAG, 선행 FAIL→명시적 prerequisite FAIL, provider 장애→ERROR. quantity·named container·unordered selector는 의미 기반 비교한다. readiness는 observedGeneration과 원하는 replicas를 함께 확인한다. 30초 기본 한도와 주입 가능한 clock 사용. 병렬성 기본 2, 변경 probe는 직렬 실행. evidence에 원문 Secret을 보관하지 않는다.
- [x] **통과 확인:** 단위 tests PASS 및 `go test -tags=integration ./tests/integration -run TestResourceGrading -count=1 -v`. API default 값이 있는 동등한 답안과 실제 wrong image를 검증한다.
- [x] **커밋:** `feat: grade resource requirements with partial scores`.

### Task 7: Service·Ingress·NetworkPolicy 동작 검사

**Files:** Create `internal/grader/network.go`, `network_test.go`, `tests/integration/network_grading_test.go`, `tests/fixtures/network/`.

**Interfaces:** Produces `NetworkChecker` implementing Task 6 Checker. Criterion Kind는 `http,tcp,network-policy`; Args에 source namespace/labels, destination ref, port, expected connection, HTTP host/path/status/body marker를 정의한다. typed probe result는 `ProbeResult{Connected bool; HTTPStatus int; BodyMarker,FailureClass string}`.

- [x] **실패 테스트 작성:** `TestAllowAndDeny`, `TestDeadServerNotPolicyPass`, `TestDNSFailureNotDenyPass`, `TestWrongBackend`, `TestProbeCleanupAfterCancel`.
  ```go
  testutil.Equal(t, correctPolicy.Status, "PASS")
  testutil.Equal(t, allTrafficBroken.Status == "PASS", false)
  testutil.Equal(t, wrongBackend.Status, "FAIL")
  testutil.Equal(t, remainingProbePods, 0)
  ```
- [x] **실패 확인:** `go test ./internal/grader -run 'TestAllow|TestDead|TestDNS|TestWrongBackend|TestProbeCleanup' -v` 실패 확인.
- [x] **최소 구현:** 3초×최대3회, 명시된 source identity의 probe Pod, 정상 서버 대조 확인, DNS 오류 분류, 라우팅 응답 marker 검사. allow/deny 결과가 함께 성립해야 policy 완전 정답. 성공했을 때도 probe inventory 정리. 이미지를 시작 전에 준비해 image pull 실패와 학습자 오답을 구분한다.
- [ ] **통과 확인:** `go test -tags=integration ./tests/integration -run TestNetworkGrading -count=1 -v`에서 정책 없음·올바름·전부 차단·서버 다운·DNS 실패를 각각 확인한다.
- [x] **커밋:** `feat: verify network behavior without false passes`.

### Task 8: 파일·이미지·Helm·Kustomize·보안 검사

**Files:** Create grader artifacts/security 파일과 tests, `tests/integration/artifact_grading_test.go`, 고정된 `tests/fixtures/artifacts/` chart·Dockerfile·overlay.

**Interfaces:** Produces `ArtifactChecker`, `SecurityChecker` implementing Checker; `CaptureInputs(ctx,Target) (map[string]string,error)`. Kind는 `file,image,helm,kustomize,logs,job,authorization,security-context`. 제출 대상만 fingerprint하고 controller status 전체를 hash하지 않는다.

- [x] **실패 테스트 작성:** `TestHelmRequiredNotRawApply`, `TestImageActuallyRuns`, `TestOverlaySemantics`, `TestAssetSymlinkEscape`, `TestInputChange`, `TestCronScheduleNotProvedByManualJob`, `TestEffectiveIdentity`. 각 이름에 해당하는 원본 fixture와 오답을 test 파일에서 명시한다.
  ```go
  testutil.Equal(t, rawAppliedWithoutRelease.Status, "FAIL")
  testutil.Equal(t, validOverlayDifferentKeyOrder.Status, "PASS")
  testutil.Equal(t, reflect.DeepEqual(beforeInputs, afterImageChanged), false)
  testutil.Equal(t, manualJobOnlyProvesSchedule, false)
  ```
- [ ] **실패 확인:** `go test ./internal/grader -run 'TestHelm|TestImage|TestOverlay|TestAssetSymlink|TestInputChange|TestCron|TestEffective' -v` 실패 확인.
- [x] **최소 구현:** Helm의 JSON 결과와 release 검증, Kustomize 렌더링 후 API 의미 비교, image ID/digest·archive 구조·실제 실행 확인. 현재 아키텍처 이미지로 검사하며 Docker 전역 cache를 정리하지 않는다. 파일은 Task 2 경로 검증 사용. Job 실행·CronJob template·schedule observation을 다른 Kind 인자로 구분한다. SA/RBAC는 실제 credential 또는 권한 review 결과, SecurityContext는 명시 필드와 필요 시 exec 결과를 검사한다.
- [x] **통과 확인:** `go test -tags=integration ./tests/integration -run TestArtifactGrading -count=1 -v` PASS. stdout/stderr·저장 evidence에 Secret이 없고 foreign Docker sentinel이 보존된다.
- [x] **커밋:** `feat: grade application artifacts and security behavior`.

### Task 9: 문제 작성 검증과 검증 기록 무효화

**Files:** Create `internal/author/` 3개 파일·tests, `tests/integration/problem_validation_test.go`.

**Interfaces:** Consumes Problem/Freeze, Manager.Apply, Engine.Evaluate, Store.SaveValidation. Produces `author.Validator.Validate(ctx context.Context,p model.Problem,seeds []uint64) (model.ValidationRecord,error)`, `CertificateMatches(record model.ValidationRecord,problemHash,graderVersion,environmentHash string) bool`.

- [x] **실패 테스트 작성:** `TestInitialSolvedWrongReset`, `TestInitiallyPerfectRejected`, `TestStaleCertificate`, `TestValidationBlockedDuringSession`. 초기 부분 점수 2, 정답 10, 오답 6인 fixture를 준비한다.
  ```go
  testutil.Equal(t, record.Cases["initial"].MaxScore, 10)
  testutil.Equal(t, *record.Cases["initial"].Score, 2)
  testutil.Equal(t, *record.Cases["solved"].Score, 10)
  testutil.Equal(t, author.CertificateMatches(record, changedHash, engineVersion, envHash), false)
  ```
- [x] **실패 확인:** `go test ./internal/author -v` 실패 확인.
- [x] **최소 구현:** 허용된 Action dispatch, 임시 문제 namespace 묶음, 단계별 환경·grader 공용 호출. cleanup 기능은 임시 관리 inventory만으로 구현하고 Task 13에서 공통 reset에 통합한다. 각 문제의 초기 벡터·정답·오답·대표 seed·대체 정답을 기록한다. 하나라도 ERROR이면 validation 성공 처리하지 않는다.
- [x] **통과 확인:** `go test -tags=integration ./tests/integration -run TestProblemValidation -count=1 -v`. 모범 해답 적용 후의 상태가 다음 사례로 누출되지 않으며 certificate hash 변경은 재검증을 요구한다.
- [x] **커밋:** `feat: validate authored problems with the runtime grader`.

### Task 10: 준비·시작·시간·동결된 세션

**Files:** Create session prepare/start 파일·tests, `tests/integration/session_start_test.go`.

**Interfaces:** Produces `session.New(s *store.Store,env *environment.Manager,engine *grader.Engine,bankDir,stateDir string) *Service`; `Prepare(ctx,StartOptions) (model.SessionRecord,error)`, `Start(ctx,sessionID string,at time.Time) error`, `Status(ctx,sessionID string,now time.Time) (Status,error)`. `StartOptions{Mode,MockID,Domain,Difficulty,Tag string; Count int; Seed uint64; RetrySessionID string; NewSeed bool}`; `Status{Session model.SessionRecord; Elapsed,Overtime time.Duration; ClockAnomaly bool}`.

- [x] **실패 테스트 작성:** `TestPrepareHidesQuestions`, `TestBrokenInitialStateIsExpected`, `TestStartClock`, `TestWallClockAfterSleep`, `TestSecondSessionRejected`, `TestBankChangeDoesNotChangeActiveSession`.
  ```go
  testutil.Equal(t, prepared.StartedAt, (*time.Time)(nil))
  testutil.Equal(t, publicQuestionFilesBeforeStart, 0)
  testutil.Equal(t, status.Elapsed, 121*time.Minute)
  testutil.Equal(t, status.Overtime, time.Minute)
  ```
- [ ] **실패 확인:** `go test ./internal/session -run 'TestPrepare|TestBroken|TestStartClock|TestWall|TestSecond|TestBankChange' -v` 실패 확인.
- [x] **최소 구현:** 전용 환경·capability·validation 확인→문제 freeze→모든 setup·초기 결과 검증→PREPARING. 확인 후 public assets 공개·RUNNING 전환·시작 시각 저장. 의도된 CrashLoop를 플랫폼 고장으로 혼동하지 않는다. 실패 준비의 inventory는 보존해 복구할 수 있게 한다.
- [x] **통과 확인:** `go test -tags=integration ./tests/integration -run TestSessionStart -count=1 -v` PASS. 개발용 세트는 5개 fixture를 사용하며 출시 문제 수로 세지 않는다.
- [x] **커밋:** `feat: prepare reproducible sessions before starting the clock`.

### Task 11: 제출·부분 저장·retry/resubmit·공개 규칙

**Files:** Create session submit/retry/review 파일·tests, `tests/integration/submission_test.go`.

**Interfaces:** Adds Service methods `Grade(ctx,sessionID string,opts GradeOptions) (model.GradingRun,error)`, `Review(ctx,sessionID,problemID string,solution bool) (Review,error)`; `GradeOptions{ProblemID string; Retry,Resubmit bool; At time.Time}`; `Review{Runs []model.GradingRun; Explanation,ReferenceSolution string; SolutionViewed bool}`. Uses CaptureInputs from Task 8.

- [x] **실패 테스트 작성:** `TestExamDisclosure`, `TestPracticeDisclosure`, `TestRetryPreservesSubmissionTime`, `TestChangedAnswerRequiresResubmit`, `TestResubmitAppendOnly`, `TestErrorNoFinalScore`, `TestCrashDuringGrade`. 최초 제출 10:00, retry 10:02, 새 제출 10:05의 고정 시각을 사용한다.
  ```go
  testutil.Equal(t, retrySubmission.SubmittedAt, originalSubmission.SubmittedAt)
  testutil.Equal(t, resubmission.SubmittedAt, originalSubmission.SubmittedAt.Add(5*time.Minute))
  testutil.Equal(t, incomplete.Evaluation.Score, (*int)(nil))
  testutil.Equal(t, strings.Contains(examBeforeSubmit, referenceAnswer), false)
  ```
- [x] **실패 확인:** `go test ./internal/session -run 'TestExam|TestPractice|TestRetry|TestChanged|TestResubmit|TestError|TestCrash' -v` 실패 확인.
- [x] **최소 구현:** 전환 잠금·제출 intent·입력 fingerprint 저장·grader 호출·원자적 완료. retry는 새 run, resubmit은 새 submission. 최초 제출 시 input capture가 불가능했다면 unchanged를 증명할 수 없으므로 resubmit만 안내한다. input spec/UID·파일·image 변경 시 retry 거부. 종료 전후 state 확인으로 stale 결과를 확정하지 않는다. 훈련 개별 채점은 세션을 끝내지 않고 결과와 시각을 저장한다.
- [ ] **통과 확인:** (미완: 포기 후 공개는 Task 13 — 현재 `ErrReviewUnsupported`; 나머지는 검증됨) integration submission 테스트 PASS. ERROR 상태의 시험 해설 비공개, 포기 후 공개, 제출 후 열린 해설은 재제출 기록에 반영. 최고 점수 합산 없음. clock 역행 flag와 채점시간 제외 확인.
- [x] **커밋:** `feat: preserve submissions and enforce feedback timing`.

### Task 12: CLI 전체 학습 흐름과 한국어 피드백

**Files:** Create cmd/ckad main, CLI root/start/status/show/mark/grade/review/doctor/bank/reset 파일·tests; Modify Dockerfile의 workspace 실행 파일 packaging; Create `tests/e2e/cli_flow_test.go`.

**Interfaces:** Produces `cli.NewRoot(deps Dependencies,in io.Reader,out,errOut io.Writer) *cobra.Command`; `Dependencies{Session *session.Service; Environment *environment.Manager; Author *author.Validator; Store *store.Store; Now func() time.Time}`. root에서 명령을 조립하고 구성요소 구현은 각 패키지 호출에 한정한다.

- [ ] **실패 테스트 작성:** `TestEnglishPromptKoreanFeedback`, `TestModeRestrictions`, `TestMarkDoesNotGrade`, `TestCLIExitCodes`, `TestTwoTerminalRead`, `TestNoPrivateContentInErrors`. 실제 명령 인자·입력·출력을 기록한다.
  ```go
  testutil.Equal(t, strings.Contains(questionOutput, "Namespace"), true)
  testutil.Equal(t, strings.Contains(failureOutput, "요구사항"), true)
  testutil.Equal(t, gradeCallsAfterMark, 0)
  testutil.Equal(t, codeForUnmetLearningCriteria, 1)
  ```
- [ ] **실패 확인:** `go test ./internal/cli -v`에서 누락된 명령/공개 규칙 실패 확인.
- [ ] **최소 구현:** spec 10장의 명령·옵션 모두 연결. exit 0=성공한 조작 또는 만점 채점, 1=정상 채점의 미충족, 2=사용법/모드 오류, 3=환경·저장·채점 불가. 모의시험 참고 66% 표시는 100점과 별도이며 합격 예측 문구 없음. solution은 명시 열람 후만 출력. stdout에는 사용자 결과, 진단은 stderr. context 취소를 모든 작업으로 전달한다.
- [ ] **통과 확인:** `go test ./internal/cli -v`; `go test -tags=e2e ./tests/e2e -run TestCLIFlow -count=1 -v` PASS. 5개 개발 fixture로 start→풀이→grade→review가 동작한다. reset 연결은 Task 13 완료 전에는 미구현 오류를 명확히 반환하며 완료로 표시하지 않는다.
- [ ] **커밋:** `feat: expose the terminal training workflow`.

### Task 13: 문제별 reset·전체 복구·재도전

**Files:** Create environment cleanup, session reset/recover 및 tests, `tests/integration/reset_test.go`; Modify author validation cleanup과 CLI reset wiring.

**Interfaces:** Adds `Manager.Cleanup(ctx,id model.Identity,inventory model.Inventory) error`; `Service.Reset(ctx,sessionID string,opts ResetOptions) error`, `Recover(ctx,environmentID string) error`; `ResetOptions{ProblemID string; Cluster bool; ConfirmedEnvironmentID string}`. 기존 StartOptions.RetrySessionID/NewSeed로 retry 세션 생성.

- [ ] **실패 테스트 작성:** `TestResetNamespaceBundle`, `TestPVCDataRemoved`, `TestFinalizerIsNotForced`, `TestPersonalFilesPreserved`, `TestRetryOneWrongProblem`, `TestCrashAfterDelete`, `TestForeignResourcesPreserved`.
  ```go
  testutil.Equal(t, strings.Contains(newVolumeContents, "old-attempt-marker"), false)
  testutil.Equal(t, personalAliasAfter, personalAliasBefore)
  testutil.Equal(t, len(retry.Problems), 1)
  testutil.Equal(t, foreignSentinelAfter, foreignSentinelBefore)
  ```
- [ ] **실패 확인:** `go test ./internal/session ./internal/environment -run 'TestReset|TestPVC|TestFinalizer|TestPersonal|TestRetryOne|TestCrashAfter|TestForeign' -v` 실패 확인.
- [ ] **최소 구현:** UID·owner 검증→정리 intent→namespaced 묶음 삭제 완료 대기→소유 cluster-scoped/스토리지/이미지 정리→새 attempt 작업 폴더. 과거 풀이·점수·개인 설정은 archive로 보존한다. platform 공유 리소스와 다른 Docker 리소스는 제외. 현재 시험의 개별 reset 거부, 전체 포기 확인. `--cluster`는 별도 전용 environment ID 확인 후만 재생성한다. 중단된 단계는 실제 상태를 확인해 재개한다.
- [ ] **통과 확인:** `go test -tags=integration ./tests/integration -run TestReset -count=1 -v`와 CLI 전체 flow PASS. 정리 실패 상태에서 새로운 시험을 덮어 시작하지 않는다. 새 seed와 동일 seed 재도전, missing package 거부 확인.
- [ ] **커밋:** `feat: reset owned practice state without losing user work`.

## 4. 제품 문제은행: 20문항 구성

다음은 자체 문제의 출제 목적과 점수다. 실제 영어 문제·해설·검사 정의는 각 Task에서 작성한다. initial 점수는 문제별로 명시하며 모든 항목을 억지로 초기 FAIL로 만들지 않는다. 각 검사는 영어 요구사항의 항목에 연결한다.

| Task | ID | 점수 | 핵심 과제·주요 잘못된 풀이 |
| --- | --- | ---: | --- |
| 14 | `image-001` | 4 | 이미지 빌드·산출물·실행 / tag만 만들고 잘못된 실행 내용 |
| 14 | `batch-001` | 4 | Job·CronJob workload와 성공 조건 / manual Job으로 schedule을 대체 |
| 14 | `daemonset-001` | 4 | workload 선택·DaemonSet 노드 배치 / Deployment로 대체 |
| 14 | `multicontainer-001` | 4 | init·sidecar·공유 임시 볼륨 / 컨테이너는 있으나 데이터 공유 실패 |
| 14 | `volume-001` | 4 | PVC 연결·데이터 유지 / emptyDir로 영속성 대체 |
| 15 | `rollout-001` | 5 | broken rollout 복구·요구 version/전략 / 준비되지 않은 replica 포함 |
| 15 | `release-001` | 5 | canary workload·Service 대상 구성 / 안정 version 또는 canary가 누락 |
| 15 | `helm-001` | 5 | 제공 chart 배포·values 수정 / manifest만 직접 apply |
| 15 | `kustomize-001` | 5 | base 유지·overlay 수정·배포 / base를 직접 변경하거나 overlay 미사용 |
| 16 | `api-migration-001` | 5 | 폐기 API manifest 수정·실제 적용 / apiVersion만 바꾸고 schema 오류 |
| 16 | `probes-001` | 5 | readiness/liveness 조건과 동작 / probe 삭제로 정상처럼 보이게 함 |
| 16 | `diagnostics-001` | 5 | 로그·CLI 상태/metrics를 이용한 장애 진단·복구 / 원인 대신 symptom만 숨김 |
| 17 | `custom-resource-001` | 5 | 제공 CRD 발견·유효 custom resource 사용 / 비슷한 ConfigMap으로 대체 |
| 17 | `access-001` | 5 | ServiceAccount·Role/Binding·최소 요구 권한 / 잘못된 SA 또는 과도한 권한 |
| 17 | `resources-001` | 5 | requests·limits·quota 조건 / quota 삭제 또는 resource 조건 미충족 |
| 17 | `configuration-001` | 5 | ConfigMap·Secret 소비·앱 응답 / 값 hardcode 또는 잘못된 key |
| 17 | `security-001` | 5 | admission 조건 하 SecurityContext·capabilities / 정책을 약화해 우회 |
| 18 | `service-001` | 6 | Service 장애의 selector/port/endpoint/연결 복구 / 이름만 같은 빈 Service |
| 18 | `networkpolicy-001` | 8 | 명시한 ingress/egress 허용·차단 / 전부 차단 또는 전부 허용 |
| 18 | `ingress-001` | 6 | host/path 라우팅과 올바른 backend / 200만 주는 잘못된 앱 |

동작·수정·생성·진단 유형을 각 문제에 명시한다. `release-001`은 정확한 요청 비율을 무작위 HTTP 표본으로 채점하지 않고 선언된 workload·endpoint membership과 양쪽 동작을 검증한다. 실제 확률적 traffic split이 요구되는 새 문제는 별도 설계한다. CRD 문제는 제공된 확장 API 사용을 검증하며 Operator 자체 개발을 요구하지 않는다.

### Task 14: Design & Build 5문제 / 20점

**Files:** Create 위 Task 14 ID 다섯 개의 `problems/<id>/` 전체 구조와 `tests/integration/bank_build_test.go`; Create `curriculum/coverage.yaml`.

**Interfaces:** Consumes schema v1과 Validator.Validate. Produces 검증된 5 package와 Design & Build 역량 연결. 공통 tests는 `loadGroup(t,ids []string)`와 `validateGroup(t,problems []model.Problem)`를 이 test 파일에 정의하고 다른 batch가 쓸 공통 부분만 `tests/integration/bank_helpers_test.go`로 이동한다.

- [ ] **실패 테스트 작성:** `TestBuildBank`가 빈 목록·배점·역량·오답 rejection을 확인하도록 작성한다. 모든 package의 검증 결과 `Passed=true`, 점수 합20, 개수5를 assert한다.
  ```go
  testutil.Equal(t, len(problems), 5)
  testutil.Equal(t, sumPoints(problems), 20)
  for _, record := range records { testutil.Equal(t, record.Passed, true) }
  ```
- [ ] **실패 확인:** `go test -tags=integration ./tests/integration -run TestBuildBank -count=1 -v`에서 문제 부재/잘못된 사례 실패 확인.
- [ ] **문제 작성:** 영어 요구사항·한국어 진단과 해설·초기값·정답·오답·대표 seed를 작성한다. Linux 두 아키텍처 이미지와 작은 resource budget 사용. 실제 CronJob 발동 검사는 최대120초의 명시적 관찰 창을 쓰며 schedule template 검사와 구분한다. 영속 볼륨은 Pod 재생성 후 보존과 reset 후 제거를 모두 검사한다.
- [ ] **통과 확인:** `TestBuildBank` PASS. 문항·검사 사이 숨은 조건 없음, 대체 답안과 reset 후 initial 점수 일치. 아직 전체 mock을 완료했다고 표시하지 않는다.
- [ ] **커밋:** `content: add validated design and build problems`.

### Task 15: Application Deployment 4문제 / 20점

**Files:** Create Task 15 ID 네 package, `tests/integration/bank_deployment_test.go`; Modify coverage.

**Interfaces:** Consumes Task 8 artifact checker와 Task 9 Validator. Produces Deployment 역량 package.

- [ ] **실패 테스트 작성:** `TestDeploymentBank`: count4, total20, Helm raw-apply 오답, overlay/base 보호, canary 누락과 rollout 미완료를 실패로 assert한다.
  ```go
  testutil.Equal(t, len(problems), 4)
  testutil.Equal(t, sumPoints(problems), 20)
  testutil.Equal(t, helmBypass.Status, "FAIL")
  ```
- [ ] **실패 확인:** `go test -tags=integration ./tests/integration -run TestDeploymentBank -count=1 -v` 실패 확인.
- [ ] **문제 작성:** 외부 chart repository 장애를 채점에 섞지 않도록 검토된 자체 작은 chart를 제공한다. Helm 값·release와 최종 상태를 모두 요구사항에 명시한다. rollout/rollback은 필요한 목표 상태·revision evidence를 정의하고 과거 명령 이력을 추측하지 않는다.
- [ ] **통과 확인:** `TestDeploymentBank` PASS; base를 훼손하지 않는 다른 유효 overlay와 다른 명령으로 같은 목표를 이룬 풀이를 인정한다.
- [ ] **커밋:** `content: add validated deployment problems`.

### Task 16: Observability & Maintenance 3문제 / 15점

**Files:** Create Task 16 ID 세 package, `tests/integration/bank_observability_test.go`; Modify coverage.

**Interfaces:** Consumes resource/ready/logs checker와 platform metrics. Produces 5개 공식 관찰·유지보수 역량에 매핑된 3 package.

- [ ] **실패 테스트 작성:** `TestObservabilityBank`: count3,total15, probe 제거·잘못된 API 변환·증상만 숨긴 풀이 실패를 assert한다.
  ```go
  testutil.Equal(t, sumPoints(problems), 15)
  testutil.Equal(t, deletedProbe.Status, "FAIL")
  testutil.Equal(t, fixedManifestAndBehavior.Status, "PASS")
  ```
- [ ] **실패 확인:** `go test -tags=integration ./tests/integration -run TestObservabilityBank -count=1 -v` 실패 확인.
- [ ] **문제 작성:** 오류 로그와 상태·metrics가 진단에 유용한 원인을 fixture에 심는다. CLI 명령 사용 이력을 강제 채점하지 않는다. API migration은 현재 v1.37에서 실제 받아들여지는 객체를 검사한다. probe 테스트는 플랫폼을 흔드는 무차별 kill을 쓰지 않는다.
- [ ] **통과 확인:** `TestObservabilityBank` PASS; 예상 고장 초기 상태는 정상적으로 출제되고 원인이 해결됐을 때만 만점이다.
- [ ] **커밋:** `content: add validated troubleshooting problems`.

### Task 17: Environment / Configuration / Security 5문제 / 25점

**Files:** Create Task 17 ID 다섯 package, `tests/integration/bank_security_test.go`; Modify coverage.

**Interfaces:** Consumes security-context/authorization·resource·network checker와 scoped CRD setup. Produces 해당 영역의 8개 공식 역량 매핑.

- [ ] **실패 테스트 작성:** `TestSecurityBank`: count5,total25, secret 비노출, RBAC 권한 부족·과잉, admission 약화, quota 제거, CRD 부재를 검사한다.
  ```go
  testutil.Equal(t, sumPoints(problems), 25)
  testutil.Equal(t, overprivileged.Status, "FAIL")
  testutil.Equal(t, strings.Contains(reportText, secretValue), false)
  ```
- [ ] **실패 확인:** `go test -tags=integration ./tests/integration -run TestSecurityBank -count=1 -v` 실패 확인.
- [ ] **문제 작성:** least privilege가 채점 조건이면 허용·불허 행동을 영어로 명시한다. CRD fixture는 관리자가 제공하고 명시 소유권으로 reset한다. namespace admission 정책과 workload SecurityContext를 함께 검증해 정책 제거 우회를 막는다. 원문 Secret을 실패 메시지에 포함하지 않는다.
- [ ] **통과 확인:** `TestSecurityBank` PASS. 8개 역량의 누락 없음과 다른 유효 리소스 구성의 정답 인정 확인.
- [ ] **커밋:** `content: add validated configuration and security problems`.

### Task 18: Networking 3문제 / 20점과 기본 모의시험

**Files:** Create Task 18 ID 세 package, `tests/integration/bank_network_test.go`, `exams/mock-01.yaml`, `tests/e2e/mock_composition_test.go`; Modify coverage.

**Interfaces:** Consumes Task 7 checker·앞선 17 package. Produces 검토된 mock-01 문제 순서·배점·총시간과 완전한 coverage mapping.

- [ ] **실패 테스트 작성:** `TestNetworkBank`, `TestMockComposition`: 3개20점, 전체20개100점, domain20/20/15/25/20, duration120분, 네 유형 모두 포함을 확인한다.
  ```go
  testutil.Equal(t, len(mock.Problems), 20)
  testutil.Equal(t, total, 100)
  testutil.Equal(t, domainTotals, []int{20,20,15,25,20})
  testutil.Equal(t, mock.Duration, 120*time.Minute)
  ```
- [ ] **실패 확인:** `go test -tags=integration ./tests/integration -run TestNetworkBank -count=1 -v`; `go test -tags=e2e ./tests/e2e -run TestMockComposition -count=1 -v`의 누락·불균형 실패 확인.
- [ ] **문제 작성·조합:** Service6/Policy8/Ingress6점. HTTP marker와 실제 트래픽을 함께 검증한다. cross-namespace 정책에서 해당 문제의 namespace 묶음만 관리한다. 문제 간 상호 의존·이름 충돌을 금지한다. 번호·난이도·배점은 검토된 고정 구성, parameter는 seed에 따라 변화한다.
- [ ] **통과 확인:** 위 tests PASS. 전체 문제은행의 모든 validation 기록이 현재 hash·환경·grader version에 유효하다. 공식 PDF 각 항목이 coverage에 연결됐는지 확인한다.
- [ ] **커밋:** `content: assemble the validated 100-point mock exam`.

### Task 19: 전체 흐름·중단·양쪽 실기기 검증과 사용 문서

**Files:** Create `tests/e2e/acceptance_test.go`, `tests/e2e/recovery_test.go`, `.github/workflows/ci.yaml`, `docs/usage/{mac,windows,training}.md`, `docs/authoring/problem-guide.md`, `docs/validation/acceptance-matrix.md`; Modify README, Makefile.

**Interfaces:** Consumes 전체 제품 CLI·mock-01. Produces 실제 version·architecture·memory·측정시간·증거 경로가 있는 검증 보고서와 재현 가능한 테스트 명령.

- [ ] **실패 테스트 작성:** `TestFullMockLifecycle`, `TestInterruptedOperations`, `TestNoLeaksAfterThreeCycles`. 정상 전체모범풀이100점, 문제별 대표오답 기대점수, 3회 reset후 initial 벡터, 소유하지 않은 sentinel·개인 파일 보존을 assert한다.
  ```go
  testutil.Equal(t, solvedScores, []int{100,100,100})
  testutil.Equal(t, leakedOwnedResources, 0)
  testutil.Equal(t, foreignSentinelAfter, foreignSentinelBefore)
  testutil.Equal(t, personalFilesAfter, personalFilesBefore)
  ```
- [ ] **실패 확인:** `make test-e2e`를 실행해 남은 통합 문제를 실제 실패로 수집한다. 테스트 환경 부재는 SKIPPED/NOT RUN으로 기록하고 PASS로 대체하지 않는다.
- [ ] **최소 보완·문서화:** 발견된 문제만 소유 모듈에서 수정하고 회귀 테스트를 추가한다. mac/windows 시작·복수 터미널·alias·제출·복습·reset·오류 복구를 문서화한다. bootstrap/의존성 checksum/version 사용법, Docker 소켓의 권한 범위, SSH·다중context 제외를 설명한다. CI는 unit·schema·build·가능한 native integration을 수행하며 M2 실측을 에뮬레이터 결과로 대체하지 않는다.
- [ ] **최종 검증:** `make test`, `make test-race`, `go vet ./...`, `make test-integration`, `make test-e2e` PASS. M2 8GB/4GiB Docker와 Windows16GB 각각에서 20문항 동시 준비·3회 lifecycle을 실행한다. cold/warm start·reset·grade 시간, peak memory, OOM/환경 Pending 여부를 기록한다. 실제 접근할 수 없는 장비의 셀은 미완료로 남긴다.
- [ ] **커밋:** `test: verify full mock lifecycle and document operation`. 최종 변경 검토와 사용자 실행 검증 후에만 출시 준비 완료라고 보고한다. push·merge는 별도 요청 범위다.

## 5. Spec coverage와 자체 검토

| Spec 범위 | 구현 Task | 판정 근거 |
| --- | --- | --- |
| 1–3 목적·MVP·공식 역량 | 1, 14–19 | 동일 환경, 20문항·배점·coverage, 공개 자료 기반 문제 |
| 4 환경·architecture·자원 | 1, 4, 5, 19 | native ARM64/AMD64·capability·4GiB 실측 |
| 5 내부 경계·저장 선택 | 2–12 | 공통 값 타입, API 중심, SQLite, CLI 분리 |
| 6 schema·seed·아티팩트 | 2, 8–10 | strict schema, hash·freeze, path guard, stale certificate |
| 7 문제 검증·출제 | 9, 14–18 | Initial/Solved/Wrong/Reset/Alternative와 영역 매핑 |
| 8 채점·부분 점수·오류 | 6–8, 11 | 거짓 정답 방지, ERROR 보류, evidence·타이밍 |
| 9 시간·모드·재도전 | 10–13 | sleep·오버타임·해설 gate·retry/resubmit·단일오답 |
| 10 명령 | 12, 13 | 전체 CLI와 실제 terminal flow |
| 11 저장·복구 | 3, 10, 11, 13 | transaction·operation intent·kill·동시성 |
| 12 reset·안전 | 4, 8, 13, 19 | UID/CA·inventory·PVC·개인 파일·foreign sentinel |
| 13 출시 검증 | 19 | 실제 두 장비의 3회 lifecycle과 정확한 검증 상태 |
| 14–15 후속 범위·승인 | 전 Task | 제외 범위 준수, 계획 검토 후 실행 |

자가 검토는 spec coverage, 명령·타입·method 이름의 일치, 테스트의 실패 원인, Review Focus 다섯 항목, 계획과 구현의 비율을 확인한다. 환경 검증 실패를 통과했다고 간주하거나 다음 단계에서 자동 해결될 것이라고 숨기지 않는다.

## 6. 실행 방식 선택

2026-10-14 사용자가 **Subagent-driven**을 선택했다. 작업별 구현자와 별도 리뷰어를 사용하고, 다음 의존 작업 전에 결과를 확인한다. 경계·상태·안전장치가 많은 이 계획에 맞는 방식이며 검토에 추가 시간과 컨텍스트가 든다. (대안이던 Native 방식은 현재 에이전트가 순서대로 구현하고 최종 독립 리뷰만 수행한다.)

어느 방식이든 동시에 같은 클러스터를 변경하는 Task를 병렬 실행하지 않는다. native 장비 접근이 필요한 검증은 실제 환경에서 수행하고, 불가능하면 그 완료 조건을 남긴다. 이 문서를 작성·커밋한 사실은 구현이나 테스트 통과를 의미하지 않는다.
