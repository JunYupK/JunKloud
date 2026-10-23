# JunKloud

CKAD(Certified Kubernetes Application Developer) 자격증 공부를 위한 **로컬 실습 환경 구성·문제 출제·자동 채점 프로그램**입니다.

- 전용 [kind](https://kind.sigs.k8s.io/) 클러스터(**kind v0.33 / Kubernetes v1.37**)를 만들어 실제 클러스터에서 문제를 풉니다.
- 학습 명령(`ckad`)과 셸, `kubectl`, 채점은 모두 **같은 Linux 작업 컨테이너** 안에서 실행합니다. 호스트에는 Docker Desktop만 있으면 됩니다.
- 문제·요구사항은 실제 시험처럼 **영어**로, 피드백과 해설은 **한국어**로 제공합니다.
- 채점은 실제 클러스터 상태를 관찰해 항목별 **PASS / FAIL / ERROR**로 판정합니다. 환경 장애로 판정할 수 없는 항목(ERROR)이 있으면 총점을 확정하지 않습니다.

> 현재 문제은행은 **12문제 / 55점**의 부분 은행입니다. 기본 20문항 모의시험(`mock`)은 아직 제공하지 않으므로 **훈련 모드(`--mode practice`)** 로 사용하세요. 자세한 내용은 [현재 상태와 제한](#현재-상태와-제한)을 참고하세요.

## 목차

1. [준비물](#준비물)
2. [빠른 시작](#빠른-시작)
3. [처음 한 번: 문제 검증](#처음-한-번-문제-검증)
4. [학습 흐름](#학습-흐름)
5. [명령 요약](#명령-요약)
6. [채점 규칙](#채점-규칙)
7. [초기화와 복구](#초기화와-복구)
8. [이미지 빌드 문제](#이미지-빌드-문제)
9. [경로와 환경 변수](#경로와-환경-변수)
10. [문제은행](#문제은행)
11. [개발자용](#개발자용)
12. [문제 해결](#문제-해결)
13. [현재 상태와 제한](#현재-상태와-제한)
14. [관련 문서](#관련-문서)

## 준비물

| 항목 | 내용 |
| --- | --- |
| 호스트 OS | Windows 또는 macOS(Docker Desktop), 또는 Docker Engine이 있는 Linux |
| Docker | Docker Desktop의 **Linux containers** 모드와 Docker Compose |
| 자원 | 목표 최소 사양은 Docker Linux VM 메모리 **4GiB**입니다. 아직 실측하지 않았으므로 여유가 있으면 더 크게 잡으세요. |
| 네트워크 | 처음 빌드할 때 도구와 고정된 이미지를 내려받습니다. 이후 문제 풀이는 외부 서비스에 의존하지 않습니다. |

호스트에 `kubectl`, `kind`, Helm, Go를 설치할 필요는 없습니다. 필요한 도구는 버전과 SHA-256이 고정된 상태로 작업 컨테이너에 들어 있습니다([`runtime/versions.lock.yaml`](runtime/versions.lock.yaml)).

| 도구 | 버전 |
| --- | --- |
| kind | v0.33.0 (노드 이미지 Kubernetes v1.37.0) |
| kubectl | v1.37.0 |
| Helm | v4.3.0 |
| yq | v4.54.1 |
| Docker CLI / Compose / Buildx | 29.9.0 / v5.6.0 / v0.38.0 |

## 빠른 시작

### 1. 작업 컨테이너 시작 (호스트)

저장소를 내려받은 뒤 저장소 루트에서 실행합니다.

macOS · Linux:

```sh
bash runtime/up.sh
docker compose exec workspace bash
```

Windows PowerShell:

```powershell
powershell -ExecutionPolicy Bypass -File runtime/up.ps1
docker compose exec workspace bash
```

시작 스크립트가 하는 일:
- `kind` Docker 네트워크(bridge)가 없을 때만 새로 만듭니다. 기존 네트워크는 바꾸거나 지우지 않습니다.
- `workspace` 이미지를 빌드하고 컨테이너를 띄웁니다. 저장소는 컨테이너의 `/src`에 연결됩니다.
- 학습자 홈(`/home/learner`)은 `learner-home` 볼륨에 보존됩니다. `.bashrc`, 별칭, 풀이 파일, 기록이 컨테이너를 다시 만들어도 남습니다. `docker compose down -v`를 쓸 때만 삭제됩니다.

다른 터미널에서도 `docker compose exec workspace bash`로 같은 컨테이너에 들어가 같은 세션을 볼 수 있습니다. 이것은 SSH 접속 훈련이 아니라 일반 터미널 접속입니다.

### 2. 환경 점검 (컨테이너 안)

```sh
ckad doctor
```

처음 실행하면 JunKloud 전용 kind 클러스터를 만들고 다음을 점검합니다.
- 도구 버전
- DNS, Service, Ingress, NetworkPolicy, 저장소
- metrics(`kubectl top`), 이미지 실습 기능

컨테이너 안의 `kubectl`은 자동으로 이 전용 클러스터의 kubeconfig를 사용합니다. 호스트의 kubeconfig는 쓰지 않습니다.

### 3. 문제 검증 → 훈련 시작

```sh
# 처음 한 번 (아래 "처음 한 번: 문제 검증" 참고)
for id in image-001 batch-001 daemonset-001 multicontainer-001 volume-001 \
          rollout-001 release-001 helm-001 kustomize-001 \
          api-migration-001 probes-001 diagnostics-001; do
  ckad bank validate "$id" || break
done

ckad list                                  # 출제 가능한 문제 확인
ckad start --mode practice --count 5       # 5문제 훈련 준비 → yes 입력으로 시작
```

## 처음 한 번: 문제 검증

JunKloud는 **현재 환경에서 검증을 통과한 문제만 출제**합니다. 검증은 문제마다 다음 단계를 실제 클러스터에서 확인합니다.

1. **초기 상태:** 문제를 막 낸 상태가 미해결이고, 정의된 초기 점수와 일치합니다.
2. **모범 풀이:** 만점이 나옵니다.
3. **대표 오답:** 지정한 항목만 정확히 실패합니다.
4. **동등한 다른 정답:** 만점이 나옵니다.
5. **reset:** 이전 실행이 다음 실행에 영향을 주지 않습니다.

```sh
ckad bank validate <problem-id>
```

- 검증 기록은 문제 내용 hash, 채점기 버전, 환경 버전에 묶입니다. 셋 중 하나라도 바뀌면 다시 검증해야 합니다.
- 문제 하나에 수 분에서 수십 분이 걸립니다. 예를 들어 `probes-001`은 의도적으로 긴 안정성 관찰 때문에 오래 걸립니다.
- 진행 중인 훈련·시험 세션이 있으면 검증을 거부합니다.
- `ckad list`로 출제 가능한 문제와 제외된 문제, 그 이유를 확인할 수 있습니다.

## 학습 흐름

### 훈련 시작

```sh
ckad start --mode practice --count 5
```

- 문제 수는 **5~20개**입니다. 검증된 문제 중에서 선택합니다.
- 필터 옵션
  - `--domain`: `design-build`, `deployment`, `observability`, `environment-security`, `networking`. 뒤의 두 영역은 아직 문제가 없습니다.
  - `--difficulty`, `--tag`
  - `--seed`: 같은 문제 선택을 재현합니다.
- 준비가 끝나면 `준비 완료: ... 시작하시겠습니까? [yes/no]`가 나옵니다. `yes`를 입력하면 시계가 시작됩니다.
- 준비 단계에서 끊기거나 `no`를 입력해도 준비 상태는 보존됩니다. `ckad start`로 시작 확인을 다시 할 수 있습니다.
- 한 영역만 고르면 5문제가 안 될 수 있습니다(예: `observability`는 3문제). 이때는 이유와 가능한 수를 보여 주고 시작하지 않습니다. 부족분을 중복으로 몰래 채우지 않습니다.

### 문제 풀기

```sh
ckad status            # 세션 상태, 경과 시간, 문제 목록(Q1, Q2 ...)
ckad show Q1           # 영어 문제, context, namespace, 제공 파일 위치
cd ~/ckad/<session>/<problem>/attempt-1   # 문제별 작업 폴더 (question.md와 제공 파일)
kubectl get pods -n <namespace>
ckad mark Q3 --review  # 다시 볼 문제 표시 (채점에는 영향 없음), 해제는 --clear
```

- 작업 컨테이너에서 `kubectl`, Helm, `yq`, `curl`, `nano`, `less`, `git`, Docker CLI를 쓸 수 있습니다.
- 실제 시험처럼 `k` 별칭(`alias k=kubectl`)과 `kubectl`·`k`의 Bash 자동완성(Tab)이 모든 대화형 셸에 미리 설정돼 있습니다([`runtime/kubectl-shell.bash`](runtime/kubectl-shell.bash)).
  - `/etc/bash.bashrc`에서 불러오므로, 이미 쓰던 `learner-home` 볼륨도 이미지를 다시 빌드하면 바로 적용됩니다(`bash runtime/up.sh`는 `--build`로 다시 빌드합니다).
  - 리소스 이름 자동완성(`k get pods <Tab>`)은 클러스터에 물어보므로 `ckad doctor`나 `ckad start`로 클러스터가 준비된 뒤에 동작합니다.
- 개인 별칭과 편집기 설정은 `~/.bashrc`, `~/.bash_aliases`에 추가하면 계속 유지됩니다. 기본 `k` 별칭도 여기서 덮어쓸 수 있습니다.
- 제한 시간을 넘겨도 제출할 수 있습니다. 초과 시간은 따로 표시됩니다.

### 채점과 복습

```sh
ckad grade --problem Q1            # 훈련: 한 문제만 채점
ckad grade                         # 세션 전체 제출
ckad review                        # 결과와 실패 근거
ckad review --problem Q1 --solution   # 해설·모범 풀이 열람 (열람 기록이 남습니다)
```

- **채점 출력:** 항목별 PASS/FAIL/ERROR, 배점, 기대값과 관찰값, 관찰 시각, 대상 UID, generation이 나옵니다.
- **채점 중 주의:** 답안을 수정하지 마세요. 채점 도중 대상이 바뀐 것이 감지되면 그 결과는 확정하지 않습니다.
- **복습 순서:** 실패한 요구사항 → 실제 관찰 결과 → 진단 명령과 문서 → 원인 설명 → 모범 풀이 순으로 보여 줍니다.
- **해설 열람:** 해설은 사용자가 `--solution`으로 직접 열 때만 공개합니다.
- **같은 제출 다시 채점:** ERROR가 난 제출을 답안을 고치지 않은 채 다시 관찰하려면 `ckad grade --retry`를 씁니다.
- **새 제출:** 답안을 고친 뒤에는 `ckad grade --resubmit`으로 새 제출을 기록합니다.

### 오답 재도전

```sh
ckad start --retry <session-id> --failed              # 같은 문제 version·조건으로 재도전
ckad start --retry <session-id> --failed --new-seed   # 검증된 범위의 새 조건으로 재도전
```

오답 재도전은 1~20문제까지 허용합니다. 원래 문제 패키지가 없어졌으면 재도전을 거부합니다.

## 명령 요약

| 명령 | 설명 |
| --- | --- |
| `ckad doctor` | 전용 환경, 도구 버전, 플랫폼 기능(capability) 점검 |
| `ckad list` | 출제 가능한 문제·모의시험 조회 |
| `ckad start [mock-id]` | 환경과 문제를 준비하고 확인 후 시작. `--mode practice\|mock`(기본 `mock`), `--count`, `--domain`, `--difficulty`, `--tag`, `--seed`, `--retry <session-id> --failed [--new-seed]` |
| `ckad status` | 학습 모드, 문제 목록, 시간, 복구가 필요한 작업 |
| `ckad show <Q번호\|problem-id>` | 영어 문제, 대상 context, namespace, 제공 파일 |
| `ckad mark <Q번호> --review\|--clear` | 다시 볼 문제 표시·해제 |
| `ckad grade [--problem Q1] [--retry\|--resubmit]` | 전체 제출 또는 훈련 문제 채점 |
| `ckad review [session-id] [--problem Q1] [--solution]` | 제출 결과, 실패 근거, 해설 |
| `ckad reset [--problem Q1\|--cluster\|--recover]` | 소유 리소스 정리. 풀이와 기록은 보존 |
| `ckad image build --problem Q1 --tag <ref> [--context .]` | 현재 풀이의 Dockerfile로 소유권이 기록되는 이미지 빌드 |
| `ckad bank validate <problem-id>` | 문제 패키지 검증(초기, 정답, 대표 오답, reset, parameter 범위) |

공통 옵션은 `--bank-dir`, `--state-dir`, `--workspace`입니다([경로와 환경 변수](#경로와-환경-변수) 참고). 명령별 자세한 옵션은 `ckad <명령> --help`로 확인하세요.

## 채점 규칙

| 판정 | 의미 | 점수 |
| --- | --- | --- |
| PASS | 요구사항 충족 | 해당 배점 획득 |
| FAIL | 풀이 결과가 요구사항을 충족하지 않음. 잘못된 이미지, selector, probe 때문에 앱이 실패한 경우도 포함 | 0점과 실제 관찰 근거 |
| ERROR | 클러스터 API 불통, 검사 도구 준비 실패 등으로 판정할 수 없음 | 점수 확정 보류 |

- **부분 점수:** 항목마다 전부 아니면 0점이고, 문제 점수는 통과한 항목 배점의 합입니다.
- **ERROR가 있을 때:** 해당 제출은 미완료입니다. 확정 총점과 합격 여부를 표시하지 않습니다.
- **선행 항목:** 선행 항목이 실패하면 딸린 항목은 `prerequisite not satisfied`로 FAIL입니다.
- **의미 비교:** 예를 들어 `1` CPU와 `1000m`은 같은 값으로 봅니다. 문제가 요구하지 않는 한 YAML 키 순서, 문법, 임의의 라벨 이름으로 정답을 제한하지 않습니다.
- **숨은 조건 없음:** 채점 항목은 모두 영어 문제 문장에 적혀 있습니다. 명령 사용 이력(`kubectl top`을 실행했는지 등)은 채점하지 않습니다.
- **기다리는 시간:** 상태가 수렴할 때까지 기본 30초를 기다립니다. 네트워크 요청은 1회 3초, 최대 3회입니다. 더 긴 관찰이 필요한 문제는 문제에 그 한도가 적혀 있습니다.
- **모의시험 66%:** 공개된 합격 기준을 참고용으로만 표시합니다. 공식 합격 예측이 아닙니다.

`ckad` 종료 코드:

| 코드 | 의미 |
| --- | --- |
| 0 | 조작 성공 또는 만점 |
| 1 | 정상 채점이지만 요구사항 미충족 |
| 2 | 사용법·모드 오류 |
| 3 | 환경·저장·채점 불가 |

## 초기화와 복구

```sh
ckad reset --problem Q1   # 훈련 문제 하나 초기화 → 새 attempt 폴더 생성
ckad reset                # 세션 포기 + 소유 리소스 전체 정리 (환경 ID 입력으로 확인)
ckad reset --cluster      # 전용 클러스터 재생성 (환경 ID 입력으로 확인)
ckad reset --recover      # 중단된 초기화·이미지 빌드 기록을 이어서 정리
```

- **보존되는 것:** 이전 풀이 폴더, 점수, 개인 설정은 지우지 않습니다. 정리 대상은 JunKloud가 만들고 소유를 기록한 리소스뿐입니다.
- **건드리지 않는 것:** 다른 Docker 리소스는 변경하거나 삭제하지 않습니다. 전역 `prune`도 쓰지 않습니다.
- **모의시험 중:** 진행 중인 모의시험의 문제 하나만 초기화하는 것은 거부합니다.
- **정리 실패:** 정리를 확인할 수 없는 PVC/PV나 finalizer가 있으면 오류와 기록을 남깁니다. 정리가 끝나지 않은 동안에는 새 학습을 시작하지 않습니다.

## 이미지 빌드 문제

`image-001`처럼 이미지를 만드는 문제는 관리 빌드 명령을 쓰면 reset 때 자동으로 정리됩니다.

```sh
ckad image build --problem Q1 --context . --tag junkloud.local/app:practice
docker save --output image.tar junkloud.local/app:practice   # 아카이브가 필요할 때
```

- **context 경로:** 현재 attempt 폴더 기준입니다.
- **빌드 조건:** 제한 시간 5분, 빌드 중 네트워크 없음입니다. 베이스 이미지는 제공된 고정 이미지(또는 `scratch`)만 쓸 수 있습니다.
- **Dockerfile 제한:** `FROM` 한 단계와 로컬 `COPY`만 지원합니다.
- **일반 빌드도 채점됩니다:** 일반 `docker build`로 만든 이미지도 같은 기준으로 채점합니다. 다만 소유가 기록되지 않아 reset 때 자동 정리되지 않고, 오류로 남겨 알려 줍니다.

자세한 규칙은 [`runtime/README.md`의 Image practice ownership](runtime/README.md#image-practice-ownership)을 참고하세요.

## 경로와 환경 변수

| 용도 | 기본값 | 환경 변수 | 옵션 |
| --- | --- | --- | --- |
| 문제은행 루트(`problems/`, `exams/`) | `/src` | `CKAD_BANK_DIR` | `--bank-dir` |
| 상태(세션 DB, kubeconfig, 기록) | `$HOME/.local/share/junkloud` | `CKAD_STATE_DIR` | `--state-dir` |
| 풀이 작업 폴더 | `$HOME/ckad` | `CKAD_WORKSPACE` | `--workspace` |

- 풀이 폴더 구조는 `<workspace>/<session>/<problem>/attempt-<n>/`입니다. 문제를 초기화하면 다음 번호의 attempt 폴더가 생기고, 이전 attempt는 그대로 남습니다.
- 셸에서 상태 경로를 바꿨다면 `kubectl`도 맞춰 주세요.
  ```sh
  export KUBECONFIG="$CKAD_STATE_DIR/environment/kubeconfig"
  ```
- 개발용 fixture 은행: `CKAD_BANK_DIR=/src/tests/fixtures/dev-bank`. fixture의 `dev-mock`은 제품 모의시험이 아닙니다.

## 문제은행

| ID | 영역 | 유형 | 점수 | 주제 |
| --- | --- | --- | ---: | --- |
| `image-001` | Design & Build | build | 4 | 명령을 실행 가능한 이미지로 패키징 |
| `batch-001` | Design & Build | build | 4 | Job 실행과 CronJob 일정 |
| `daemonset-001` | Design & Build | build | 4 | DaemonSet으로 노드 에이전트 배치 |
| `multicontainer-001` | Design & Build | build | 4 | init·sidecar 컨테이너와 임시 볼륨 |
| `volume-001` | Design & Build | modify | 4 | Pod를 교체해도 데이터 유지(PVC) |
| `rollout-001` | Deployment | troubleshoot | 5 | 멈춘 rollout 복구 |
| `release-001` | Deployment | build | 5 | stable·canary 백엔드 구성 |
| `helm-001` | Deployment | build | 5 | 로컬 Helm chart와 release values |
| `kustomize-001` | Deployment | build | 5 | base를 건드리지 않는 overlay 배포 |
| `api-migration-001` | Observability | modify | 5 | 폐기된 API manifest를 제공되는 API로 이전 |
| `probes-001` | Observability | troubleshoot | 5 | readiness·liveness·startup probe 수정 |
| `diagnostics-001` | Observability | troubleshoot | 5 | CrashLoop 원인 진단과 CPU 사용량 확인(`kubectl top`) |

- **영역별 합계:** Design & Build 5문제/20점, Deployment 4문제/20점, Observability 3문제/15점입니다.
- **영역 매핑:** [`curriculum/coverage.yaml`](curriculum/coverage.yaml)에 있습니다.
- **필요한 기능(capability):** `image-001`은 `image-build`, `volume-001`은 `storage`, `diagnostics-001`은 `metrics`가 필요합니다. 환경이 이 기능을 지원하지 않으면 해당 문제는 출제하지 않습니다.
- **변형:** 문제마다 parameter(namespace 등)가 seed에 따라 바뀝니다.

### 문제 패키지 구조

```text
problems/<problem-id>/
  problem.yaml        # 메타데이터, parameter, 채점 항목(criteria), setup/solution/cases 동작, 초기 점수
  question.en.md      # 영어 문제
  explanation.ko.md   # 한국어 해설: 핵심 개념, 진단 순서, 풀이, 공식 문서
  setup/              # 초기 리소스. setup/provided/ 아래 파일은 학습자 작업 폴더로 제공
  solution/           # 모범 풀이
  cases/              # 대표 오답과 동등한 다른 정답
```

- **형식:** 정의 파일은 알려진 검사·작업 종류만 허용합니다. 임의 코드나 shell 실행은 허용하지 않습니다.
- **스키마:** [`schemas/problem-v1.schema.json`](schemas/problem-v1.schema.json)
- **Helm·Kustomize 작업 규칙:** [`docs/author-tool-actions.md`](docs/author-tool-actions.md)
- **작성 시 주의:** setup이 만든 객체는 Create로 생성됩니다. 그래서 오답 사례에서 이후 server-side apply로 필드를 지울 수 없습니다. 필드를 지운 상태를 만들려면 `delete-owned`로 지운 뒤 `apply`하세요.

## 개발자용

개발 컨테이너에는 Go 컴파일러와 race 검사용 C 도구가 추가로 들어 있습니다.

```sh
bash runtime/up.sh dev                       # Windows: runtime/up.ps1 -Service dev
docker compose exec dev bash
```

컨테이너 안에서:

```sh
make test              # go test ./...
make test-race         # go test -race ./...
make test-runtime      # 작업 환경 자체 검증 (Linux, 볼륨, 고정 도구 버전)
make test-integration  # 실제 kind 클러스터를 만들고 지우는 통합 테스트
make test-e2e          # ckad CLI E2E (25분 상한)
go vet ./... && go vet -tags=integration ./...
```

문제은행 통합 테스트(실제 클러스터 검증) 예:

```sh
go test -tags=integration ./tests/integration -run '^TestObservabilityBankSmoke$' -count=1 -timeout=30m -v
go test -tags=integration ./tests/integration -run '^TestObservabilityBank$' -count=1 -timeout=150m -v
# 한 문제만: -run '^TestObservabilityBank$/^probes-001$'
```

다른 문제은행 테스트(`TestBuildBank`, `TestDeploymentBank` 등)도 같은 방식으로 실행합니다.

호스트가 표준과 다를 때 쓰는 테스트 전용 환경 변수입니다. 제품 기본값을 바꾸지 않습니다.

| 변수 | 쓰는 경우 |
| --- | --- |
| `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1` | cgroup v1 호스트, `oom_score_adj` 조정이 막힌 VM |
| `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6` | 위 조건에 더해 엔진 커널의 IPv6가 꺼져 있을 때 |
| `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy` | 커널이 NetworkPolicy 차단을 지원하지 않음을 미리 알 때. doctor가 FAIL로 보고하는지 확인하고, 해당 테스트는 건너뛴 것으로 표시 |

- E2E와 통합 테스트는 실제 Docker, kind, kubectl과 클러스터 자원이 필요합니다.
- 사용할 수 없으면 실패를 반환합니다. 건너뛴 검증을 통과로 표시하지 않습니다.

## 문제 해결

| 증상 | 확인할 것 |
| --- | --- |
| `ckad start`가 모의시험을 찾지 못함 | 아직 제품 모의시험이 없습니다. `--mode practice`를 붙이세요. |
| `ckad list`에 문제가 없거나 제외됨 | `ckad bank validate <id>`로 현재 환경에서 검증했는지, `ckad doctor`에서 필요한 기능이 PASS인지 확인하세요. 채점기나 환경 버전이 바뀌면 다시 검증해야 합니다. |
| 훈련 시작 시 "문제 수 부족" | 훈련은 최소 5문제입니다. 필터(`--domain` 등)를 줄이거나 검증된 문제를 늘리세요. |
| 채점 결과가 ERROR | `ckad doctor`로 클러스터와 기능 상태를 확인한 뒤, 답안을 고치지 않았다면 `ckad grade --retry`를 실행하세요. |
| 명령이 중단됐거나 상태가 이상함 | `ckad status`가 기록된 미완료 작업과 복구 방법을 보여 줍니다. 초기화가 중단됐다면 `ckad reset --recover`를 실행하세요. |
| `kubectl`이 엉뚱한 클러스터에 접속 | 작업 컨테이너 안에서 실행하는지, `KUBECONFIG`가 `$CKAD_STATE_DIR/environment/kubeconfig`인지 확인하세요. |
| `kind` 네트워크 오류로 시작 실패 | 기존 `kind` 네트워크가 bridge가 아니면 시작 스크립트가 중단합니다. Docker 네트워크 설정을 확인하세요. |
| 클러스터가 망가짐 | `ckad reset --cluster`로 전용 클러스터만 다시 만듭니다. 풀이와 기록은 보존됩니다. |

## 현재 상태와 제한

- **문제은행:** 12문제 / 55점입니다. 남은 영역은 Environment·Configuration·Security(25점)와 Services & Networking(20점)이고, 기본 20문항 모의시험(`mock-01`)은 이 문제들이 추가된 뒤 제공됩니다.
- **검증한 환경:** Linux(amd64) 샌드박스입니다. M2(ARM64) 8GB에 4GiB Docker VM, Windows 16GB 같은 실제 목표 기기 검증은 아직 하지 않았습니다.
- **작업 컨테이너 도구:** 설계상 제공할 도구 중 `wget`과 man pages는 아직 이미지에 없습니다(`curl`은 있습니다).
- **범위 밖:** 실제 시험의 SSH 진입·복귀, 다중 클러스터 전환, 시험 감독, 브라우저 제한은 재현하지 않습니다.
- **비공식:** 점수와 부분 점수 방식은 JunKloud 자체 규칙입니다. 공식 채점 방식과 같다고 주장하지 않으며, 문제는 독창적으로 작성했습니다.

## 관련 문서

- [작업 환경 시작 안내](runtime/README.md): 시작 스크립트, 학습 명령, 이미지 실습 소유권
- [설계 문서](docs/superpowers/specs/2026-10-14-ckad-training-system-design.md): 요구사항, 채점 계약, 세션·공개 규칙
- [구현 계획](docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md): Task별 진행 기록
- [Helm·Kustomize 작성자 동작](docs/author-tool-actions.md)
