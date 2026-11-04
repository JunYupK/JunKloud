# JunKloud

CKAD(Certified Kubernetes Application Developer) 자격증 공부를 위한 **로컬 실습 환경 구성·문제 출제·자동 채점 프로그램**입니다.

- 내 컴퓨터에 전용 [kind](https://kind.sigs.k8s.io/) 클러스터(**kind v0.33 / Kubernetes v1.37**)를 만들고, 그 **실제 클러스터**에서 문제를 풉니다.
- 학습 명령(`ckad`), 셸, `kubectl`, 채점은 모두 **같은 Linux 작업 컨테이너** 안에서 실행합니다. 호스트(내 컴퓨터)에는 Docker Desktop만 있으면 됩니다.
- 문제·요구사항은 실제 시험처럼 **영어**로, 피드백과 해설은 **한국어**로 나옵니다.
- 채점은 클러스터의 실제 상태를 관찰해 항목별 **PASS / FAIL / ERROR**로 판정합니다. 환경 장애로 판정할 수 없는 항목(ERROR)이 있으면 총점을 확정하지 않습니다.
- 문제은행은 **20문제 / 100점**이고, 기본 모의시험 `mock-01`(20문제, 120분)이 있습니다.

> **이 프로그램을 처음 쓰는 분께.** 이 문서를 위에서 아래로 따라 하면 새 Mac 또는 Windows PC에서 모의시험 채점까지 갈 수 있게 썼습니다. 1~7장이 그 순서입니다. 8장 이후는 참고 자료입니다.
>
> **먼저 알아 둘 두 가지.**
> 1. 모의시험(`ckad start`)은 20문제가 **모두** 출제 가능할 때만 시작합니다. 특히 `networkpolicy-001`은 NetworkPolicy를 실제로 차단하는 호스트가 필요합니다. 내 컴퓨터가 그렇지 않으면 모의시험은 거부되고, **훈련 모드(`--mode practice`)** 로 연습해야 합니다([6장](#6-모의시험)).
> 2. Apple M2 8GB, Windows 16GB 같은 실제 기기에서의 검증은 **아직 하지 않았습니다.** 메모리 4GiB는 목표 예산이며 동작 보증이 아닙니다([14장](#14-현재-상태와-제한)).

## 한눈에 보기 (이미 준비가 끝난 뒤의 흐름)

```sh
# 호스트 터미널 (저장소 루트)
bash runtime/up.sh                      # Windows: powershell -ExecutionPolicy Bypass -File runtime/up.ps1
docker compose exec workspace bash

# 작업 컨테이너 안
ckad doctor                             # 환경 점검 (처음엔 전용 클러스터를 만듭니다)
ckad list                               # 출제 가능한 문제
ckad start --mode practice --count 5    # 5문제 훈련 (모의시험: ckad start)
ckad status ; ckad show Q1              # 목록, 문제 읽기
ckad grade --problem Q1 ; ckad review   # 채점, 복습
ckad reset                              # 끝낸 세션 정리 (환경 ID 입력)
```

## 목차

1. [준비물과 저장소 받기](#1-준비물과-저장소-받기)
2. [최초 1회 준비: 작업 컨테이너 시작](#2-최초-1회-준비-작업-컨테이너-시작)
3. [환경 점검: `ckad doctor`](#3-환경-점검-ckad-doctor)
4. [문제 검증: `ckad bank validate`](#4-문제-검증-ckad-bank-validate)
5. [연습 모드 처음부터 끝까지](#5-연습-모드-처음부터-끝까지)
6. [모의시험](#6-모의시험)
7. [이미지 빌드 문제](#7-이미지-빌드-문제)
8. [자주 겪는 문제와 해결](#8-자주-겪는-문제와-해결)
9. [명령어 요약표](#9-명령어-요약표)
10. [채점 규칙과 종료 코드](#10-채점-규칙과-종료-코드)
11. [경로와 환경 변수](#11-경로와-환경-변수)
12. [문제은행](#12-문제은행)
13. [개발자용·검증용](#13-개발자용검증용)
14. [현재 상태와 제한](#14-현재-상태와-제한)
15. [관련 문서](#15-관련-문서)

---

## 1. 준비물과 저장소 받기

### 1.1 필요한 것

| 항목 | 내용 |
| --- | --- |
| 호스트 OS | macOS 또는 Windows(Docker Desktop). Docker Engine이 있는 Linux도 됩니다. |
| Docker | Docker Desktop과 Docker Compose. **Linux containers** 모드여야 합니다(Windows 컨테이너 모드가 아님). |
| 메모리 | Docker가 쓰는 Linux VM에 **4GiB 이상**을 권장합니다. 이 4GiB는 설계 문서(§4.3)의 *초기 검증 예산*이며 실측으로 확인된 소비량이나 실행 보장이 아닙니다. 여유가 있으면 더 크게 잡으세요. |
| 디스크 | 도구·kind 노드 이미지·문제 이미지가 Docker에 내려받아집니다. 정확한 필요 용량은 아직 측정하지 않았으니 넉넉한 여유 공간을 두세요. |
| 네트워크 | 처음 빌드할 때 도구와 고정된 이미지를 내려받습니다. 이후 문제 풀이는 외부 서비스에 의존하지 않습니다. |
| Git | 저장소를 받을 때 필요합니다. |

호스트에 `kubectl`, `kind`, Helm, Go를 설치할 필요는 **없습니다.** 필요한 도구는 버전과 SHA-256이 고정된 상태로 작업 컨테이너 안에 들어 있습니다([`runtime/versions.lock.yaml`](runtime/versions.lock.yaml)).

| 도구 | 버전 |
| --- | --- |
| kind | v0.33.0 (노드 이미지 Kubernetes v1.37.0) |
| kubectl | v1.37.0 |
| Helm | v4.3.0 |
| yq | v4.54.1 |
| Docker CLI / Compose / Buildx | 29.9.0 / v5.6.0 / v0.38.0 |

### 1.2 Docker Desktop 설정

1. Docker Desktop을 설치하고 실행합니다.
2. **Linux containers** 모드인지 확인합니다. Windows에서는 트레이 아이콘 메뉴에 `Switch to Windows containers...`가 보이면 현재 Linux 모드입니다.
3. Docker Linux VM의 메모리를 4GiB 이상으로 둡니다.
   - macOS: Docker Desktop의 Settings → Resources에서 Memory를 설정합니다.
   - Windows(WSL 2 백엔드): 메모리 한도는 Docker Desktop 화면이 아니라 사용자 폴더의 `.wslconfig`로 정할 수 있습니다. 메뉴 이름과 위치는 Docker Desktop 버전에 따라 다르니 Docker 공식 문서를 확인하세요.
   - 어느 쪽이든 현재 값은 터미널에서 `docker info --format '{{.MemTotal}}'`(바이트)로 확인할 수 있습니다.
4. 터미널에서 Docker가 동작하는지 확인합니다.

   ```sh
   docker version
   docker compose version
   ```

   서버(Server) 정보까지 나오면 정상입니다. `Cannot connect to the Docker daemon` 같은 메시지가 나오면 Docker Desktop이 아직 시작 중이거나 꺼져 있는 것입니다.

기기별 자세한 안내는 [macOS](docs/usage/mac.md), [Windows](docs/usage/windows.md) 문서에 있습니다.

### 1.3 저장소 받기

```sh
git clone https://github.com/JunYupK/JunKloud.git
cd JunKloud
```

이 저장소는 줄바꿈을 LF로 고정([`.gitattributes`](.gitattributes))하므로 Windows에서 받아도 컨테이너 안의 셸 스크립트가 깨지지 않습니다. 이후의 모든 호스트 명령은 **저장소 루트**(`JunKloud` 폴더)에서 실행합니다.

---

## 2. 최초 1회 준비: 작업 컨테이너 시작

### 2.1 시작 스크립트 실행 (호스트 터미널)

macOS · Linux:

```sh
bash runtime/up.sh
```

Windows PowerShell:

```powershell
powershell -ExecutionPolicy Bypass -File runtime/up.ps1
```

이 스크립트가 하는 일:

- `kind`라는 Docker 네트워크(bridge)가 **없을 때만** 새로 만듭니다. 이미 있는 네트워크는 바꾸거나 지우지 않습니다. 기존 `kind` 네트워크가 bridge가 아니면 안전을 위해 중단합니다.
- `workspace` 이미지를 빌드하고 컨테이너를 백그라운드로 띄웁니다(`docker compose up -d --build workspace`). 저장소는 컨테이너의 `/src`에 연결됩니다.
- 학습자 홈(`/home/learner`)을 `learner-home`이라는 Docker 볼륨에 보관합니다. 셸 설정, 별칭, 풀이 파일, 학습 기록이 여기에 남습니다.

처음에는 도구와 이미지를 내려받고 이미지를 빌드하므로 **시간이 꽤 걸릴 수 있습니다.** 걸리는 시간은 아직 기기에서 측정하지 않았고 네트워크에 따라 다릅니다. 두 번째부터는 캐시를 쓰므로 보통 더 빠릅니다.

### 2.2 작업 컨테이너에 들어가기 (호스트 터미널)

```sh
docker compose exec workspace bash
```

들어가면 프롬프트가 이런 모양으로 바뀝니다(`@` 뒤의 호스트 이름은 컨테이너마다 다릅니다).

```text
[ckad] learner@3f2a9c1b7d4e:/src$
```

이제부터의 `ckad`, `kubectl`, `k` 명령은 모두 **이 컨테이너 안**에서 입력합니다. 호스트 터미널에서 `ckad`를 치면 "command not found"가 나옵니다.

### 2.3 두 번째 터미널

문제를 풀면서 다른 터미널에서 상태를 보고 싶으면 새 호스트 터미널을 열어 같은 명령을 한 번 더 실행합니다.

```sh
docker compose exec workspace bash
```

같은 컨테이너, 같은 세션입니다. `ckad status`, `ckad show`, `ckad review` 같은 조회는 여러 터미널에서 동시에 해도 됩니다. 단 `start`, `grade`, `reset`, `bank validate`처럼 상태를 바꾸는 작업은 한 번에 하나만 실행할 수 있습니다(다른 터미널에서 실행 중이면 종료 코드 3과 함께 "다른 준비·채점·검증 작업이 실행 중입니다"가 나옵니다). 이것은 SSH 접속 훈련이 아니라 일반 터미널 접속입니다.

### 2.4 나가기와 다시 들어가기

- 컨테이너 셸에서 나가려면 `exit` 또는 `Ctrl+D`. **컨테이너는 계속 실행**되고, 학습 세션과 클러스터도 그대로입니다.
- 다시 들어가려면 호스트 터미널에서 `docker compose exec workspace bash`.
- 시간이 흐르는 모의시험은 터미널을 닫아도 시계가 멈추지 않습니다(벽시계 기준).

### 2.5 컴퓨터를 재시작한 뒤

1. Docker Desktop을 실행해 엔진이 시작될 때까지 기다립니다.
2. 저장소 루트에서 시작 스크립트를 다시 실행합니다. 이미 만든 볼륨과 기록은 그대로 쓰고 컨테이너만 다시 띄웁니다.

   ```sh
   bash runtime/up.sh                      # Windows: runtime/up.ps1
   docker compose exec workspace bash
   ```
3. 컨테이너 안에서 `ckad status`로 세션을 확인합니다.
4. 클러스터가 이상하면 `ckad doctor`로 점검하고, 그래도 안 되면 `ckad reset --cluster`를 씁니다([8장](#8-자주-겪는-문제와-해결)). 재시작 뒤 클러스터가 자동으로 살아나는지는 Docker 설정에 따라 달라서 이 문서는 보장하지 않습니다.

### 2.6 컨테이너 멈추기와 완전히 지우기

| 하고 싶은 일 | 명령 (호스트, 저장소 루트) | 사라지는 것 / 남는 것 |
| --- | --- | --- |
| 컨테이너만 멈추기 | `docker compose stop` | 아무것도 지우지 않습니다. 볼륨과 클러스터가 남습니다. |
| 컨테이너 제거 | `docker compose down` | 컨테이너를 지웁니다. `learner-home` 볼륨(풀이·기록·셸 설정)과 `kind` 네트워크는 **남습니다**(`kind` 네트워크는 외부 소유라 지우지 않습니다). 다시 `bash runtime/up.sh`하면 이어서 쓸 수 있습니다. |
| 볼륨까지 모두 제거 | `docker compose down -v` | `learner-home` 볼륨이 **삭제**됩니다. 풀이 파일, 학습 기록(세션 DB, 점수, 검증 기록), `.bashrc`가 사라지며 되돌릴 수 없습니다. |

주의할 점:

- `docker compose down`/`down -v`는 **kind 클러스터를 지우지 않습니다.** 클러스터는 컨테이너 안이 아니라 Docker 엔진에 직접 만들어진 별도 컨테이너이기 때문입니다.
- 학습 환경을 깨끗이 정리하려면 `down -v` **전에** 컨테이너 안에서 `ckad reset`으로 세션 리소스를 정리하세요([8장](#8-자주-겪는-문제와-해결)).
- 전용 클러스터까지 직접 지우려면 컨테이너 안에서 환경 ID를 확인하고(`ckad doctor` 첫 줄의 `전용 환경: <ID>`) 그 ID로 만든 클러스터만 지웁니다. **다른 kind 클러스터를 지우지 않도록 이름을 꼭 확인하세요.**

  ```sh
  kind get clusters                                        # junkloud-<ID> 가 보이는지 확인
  kind delete cluster --name junkloud-<ID> --kubeconfig "$KUBECONFIG"
  docker ps -a --filter label=junkloud.io/environment-id=<ID>   # 남은 JunKloud 보조 컨테이너 확인
  ```

  이렇게 클러스터만 지우고 학습 기록(볼륨)을 남겨 두면 `ckad`가 기록된 환경이 사라졌다고 안내할 수 있습니다. 그때는 `ckad reset --cluster`로 복구합니다. 기록까지 모두 지우는 `down -v`와 함께 쓰는 것을 전제로 한 절차입니다.
- Docker 소켓 공유는 보안 격리 경계가 **아닙니다.** 작업 컨테이너는 호스트의 Docker 엔진에 접근하므로, 컨테이너 안에서 직접 입력한 Docker 명령은 호스트의 다른 Docker 리소스에도 영향을 줄 수 있습니다. JunKloud 자체는 자신이 만들고 소유를 기록한 리소스만 정리하고 전역 `prune`은 쓰지 않습니다.

---

## 3. 환경 점검: `ckad doctor`

컨테이너 안에서 실행합니다.

```sh
ckad doctor
```

**처음 실행하면 JunKloud 전용 kind 클러스터를 만들고** 이미지를 준비하므로 오래 걸릴 수 있습니다. 컨테이너 안의 `kubectl`은 자동으로 이 전용 클러스터의 kubeconfig만 사용합니다(호스트의 kubeconfig는 쓰지 않습니다).

출력 형식은 다음과 같습니다(도구 버전 줄의 이름과 값은 환경마다 다릅니다).

```text
전용 환경: <환경 ID>
<도구 이름>: <버전>
...
dns: PASS
service: PASS
network-policy: PASS
ingress: PASS
storage: PASS
metrics: PASS
image-build: PASS
```

- 첫 줄은 이 컴퓨터의 전용 환경 ID입니다. 나중에 `ckad reset` 확인 때 입력합니다.
- 이어서 도구 버전, 마지막에 플랫폼 기능(capability) 7개의 점검 결과가 나옵니다.
- **모두 PASS면** 종료 코드 0입니다. 하나라도 PASS가 아니면 종료 코드 **3**이고, 표준 오류에 `<이름> capability를 사용할 수 없습니다. ...`가 나옵니다.

각 기능이 FAIL일 때 못 하는 것:

| 기능 | 확인하는 것 | FAIL이면 |
| --- | --- | --- |
| `dns` | 클러스터 DNS | 어떤 문제도 이 기능을 출제 조건으로 선언하지는 않지만, FAIL이면 클러스터 기본 기능에 문제가 있다는 신호입니다. `ckad reset --cluster`를 고려하세요. |
| `service` | Service 라우팅 | 위와 같음 |
| `network-policy` | NetworkPolicy가 실제로 차단하는지(ingress 차단만 확인) | `networkpolicy-001` 출제 불가 → **모의시험 전체가 거부됨** |
| `ingress` | Ingress 라우팅 | `ingress-001` 출제 불가 → 모의시험 거부 |
| `storage` | 볼륨 프로비저닝 | `volume-001` 출제 불가 → 모의시험 거부 |
| `metrics` | `kubectl top`용 metrics | `diagnostics-001` 출제 불가 → 모의시험 거부 |
| `image-build` | 이미지 빌드·내보내기 | `image-001` 출제 불가 → 모의시험 거부 |

출제할 수 없는 문제는 `ckad list`의 `출제 불가:` 줄로 확인할 수 있습니다. **`network-policy`만 FAIL이면** 나머지 19문제로 훈련 모드를 쓸 수 있습니다. 이 점검은 ingress 차단만 증명하므로, 차단하는 호스트에서도 `networkpolicy-001`은 `ckad bank validate networkpolicy-001`로 따로 확인해야 합니다.

---

## 4. 문제 검증: `ckad bank validate`

JunKloud는 **현재 환경에서 검증을 통과한 문제만 출제**합니다. 검증은 문제마다 다음을 실제 클러스터에서 확인합니다.

1. **초기 상태:** 문제를 막 낸 상태가 미해결이고, 정의된 초기 점수와 일치합니다.
2. **모범 풀이:** 만점이 나옵니다.
3. **대표 오답:** 지정한 항목만 정확히 실패합니다.
4. **동등한 다른 정답:** 만점이 나옵니다.
5. **reset:** 이전 실행이 다음 실행에 영향을 주지 않습니다.

처음 한 번, 컨테이너 안에서 20개를 모두 검증합니다.

```sh
for id in image-001 batch-001 daemonset-001 multicontainer-001 volume-001 \
          rollout-001 release-001 helm-001 kustomize-001 \
          api-migration-001 probes-001 diagnostics-001 \
          custom-resource-001 access-001 resources-001 configuration-001 security-001 \
          service-001 ingress-001 networkpolicy-001; do
  ckad bank validate "$id" || break
done
```

문제 하나가 통과하면 이런 줄이 나옵니다.

```text
검증 완료: <문제 ID> | <N>개 seed | hash <문제 해시>
```

- **시간:** 문제마다 다르고 오래 걸릴 수 있습니다(예: `probes-001`은 긴 안정성 관찰이 있습니다). 문제별 소요 시간은 아직 기기에서 측정하지 않았으니 시간을 넉넉히 잡고, 터미널을 닫지 마세요.
- **반복문은 첫 실패에서 멈춥니다.** `networkpolicy-001`을 마지막에 둔 이유입니다. NetworkPolicy를 차단하지 않는 호스트에서는 이 문제만 실패하고 나머지 19개는 통과합니다. 그 경우 반복문에서 `networkpolicy-001`을 빼고 쓰면 됩니다.
- **검증 기록이 무효가 되는 경우:** 기록은 문제 내용 해시, **채점기 버전**(현재 `grader-9`), 환경 버전에 묶입니다. 셋 중 하나라도 바뀌면 다시 검증해야 합니다. 저장소를 업데이트한 뒤 채점기 버전이 바뀌었다면 이전 검증은 무효입니다.
- 진행 중인 훈련·시험 세션이 있으면 검증을 거부합니다(종료 코드 2). 먼저 세션을 끝내고 `ckad reset` 하세요.
- 검증 중에는 다른 준비·채점 작업과 동시에 실행할 수 없습니다.
- 검증이 안 된 문제는 `ckad list`의 `출제 불가` 줄에 나타납니다.

---

## 5. 연습 모드 처음부터 끝까지

연습(훈련) 모드는 문제 수를 골라 짧게 풀고, 문제별로 채점·초기화할 수 있습니다. 컨테이너 안에서 순서대로 따라 하세요.

### 5.1 출제 가능한 문제 보기

```sh
ckad list
```

한 줄에 문제 하나씩 `ID | 제목 | 영역 | 점수점` 형식으로 나오고, 이어서 `모의시험 mock-01 | 20문제 | 2h0m0s`, 그리고 출제 불가 문제가 있으면 `출제 불가: <ID> — <이유>` 줄이 나옵니다. 검증을 마치지 않았다면 대부분 `출제 불가`로 보입니다. 먼저 [4장](#4-문제-검증-ckad-bank-validate)을 끝내세요.

### 5.2 훈련 시작

```sh
ckad start --mode practice --count 5
```

- 문제 수는 **5~20개**입니다. 검증된 문제 중에서 고릅니다.
- 선택 필터: `--domain`(`design-build`, `deployment`, `observability`, `environment-security`, `networking`), `--difficulty`, `--tag`. 같은 선택을 다시 재현하려면 `--seed`.
- 한 영역만 고르면 5문제가 안 될 수 있습니다(예: `observability`는 3문제). 이때는 이유와 가능한 수를 보여 주고 시작하지 않습니다. 부족분을 몰래 채우지 않습니다.

환경과 문제를 준비한 뒤 확인을 묻습니다. 이 단계에서는 문제 본문을 보여 주지 않습니다.

```text
준비 완료: practice-<12자리 16진수> | 5문제 | 제한 <시간> | 환경 <환경 ID>
시작하시겠습니까? [yes/no]:
```

`yes`를 입력하면 시계가 시작되고 다음이 나옵니다.

```text
학습 시작: practice-<12자리 16진수>
ckad status로 목록을 보고 ckad show Q1로 문제를 여세요.
```

`no`를 입력하거나 준비 중 끊겨도 준비 상태는 보존됩니다. `ckad start`만 다시 입력하면 시작 확인부터 이어집니다.

### 5.3 상태 확인과 문제 읽기

```sh
ckad status
```

```text
세션: practice-<ID> | 모드: practice | 상태: RUNNING
경과: <시간> | 초과: <시간> | 제한: <시간>
Q1 <문제 ID> | <점수>점 | <제목>
Q2 ...
```

```sh
ckad show Q1
```

`show`는 문제 ID와 제목, `Context:`, `Namespace:`, 영어 문제 본문, 그리고 있으면 `Provided file:` 줄(제공 파일 이름과 위치)을 출력합니다. 영어로 쓰인 요구사항을 꼼꼼히 읽으세요. **채점 항목은 모두 문제 문장에 적혀 있습니다**(숨은 조건 없음).

### 5.4 풀이 위치와 `kubectl` 사용

문제마다 작업 폴더가 있고, 제공 파일과 `question.md`가 들어 있습니다.

```sh
cd ~/ckad/<세션 ID>/<문제 ID>/attempt-1
ls
```

- 파일로 답을 만들면(예: `kubectl apply -f`) 이 폴더에 두세요. 문제를 초기화하면 `attempt-2`가 새로 만들어지고 이전 `attempt-1`은 그대로 남습니다.
- 클러스터는 `kubectl`(또는 별칭 `k`)로 조작합니다. 실제 시험처럼 `k` 별칭과 Tab 자동완성이 미리 설정돼 있습니다.

  ```sh
  kubectl get pods -n <Show에 나온 namespace>
  k get pods -n <namespace>
  ```
- 문제 namespace는 `ckad show`에 나온 값을 쓰세요. `kubectl`은 전용 클러스터의 kubeconfig를 자동으로 씁니다.
- 작업 컨테이너에서 `kubectl`, Helm, `yq`, `curl`, `nano`, `less`, `git`, Docker CLI를 쓸 수 있습니다.
- 개인 별칭과 편집기 설정은 `~/.bashrc`, `~/.bash_aliases`에 추가하면 계속 유지됩니다.
- 다시 볼 문제는 `ckad mark Q3 --review`로 표시하고 `--clear`로 해제합니다. 표시는 채점에 영향을 주지 않습니다.
- 제한 시간을 넘겨도 제출할 수 있습니다. 초과 시간은 따로 표시됩니다.

### 5.5 채점하고 결과 읽기

```sh
ckad grade --problem Q1      # 훈련: 한 문제만 채점
ckad grade                   # 세션 전체를 제출
```

**채점 중에는 답안을 수정하지 마세요.** 시작할 때 표준 오류에 이 안내가 나옵니다. 채점 도중 대상이 바뀐 것이 감지되면 그 결과는 확정하지 않습니다.

출력 형식:

```text
채점 실행: <실행 ID> | 관찰: <시각> | 채점 소요: <시간>
점수: 3/5
요구사항 <문제 ID>/<항목 ID>: PASS (1점)
  관찰 시각: <시각> | UID: <객체 UID> | generation: <번호>
요구사항 <문제 ID>/<항목 ID>: FAIL (0점)
  기대: ... | 관찰: ...
  <한국어 피드백>
  관찰 근거: ...
요구사항 <문제 ID>/<항목 ID>: ERROR (0점)
  환경 또는 채점 오류로 판정할 수 없습니다. ckad doctor로 확인하세요.
```

| 판정 | 의미 |
| --- | --- |
| PASS | 요구사항 충족, 해당 배점 획득 |
| FAIL | 풀이가 요구사항을 충족하지 않음. 기대값·관찰값·한국어 피드백·관찰 근거가 같이 나옵니다. |
| ERROR | 클러스터 API 불통 등으로 **판정할 수 없음.** 점수가 아니라 "미확정"입니다. |

- ERROR가 하나라도 있으면 `점수:` 줄 대신 `채점 미완료: 확정 총점 없음 (ERROR 또는 진행 중)`이 나옵니다.
- **종료 코드:** 0 = 만점, 1 = 정상 채점이지만 일부 미충족, 3 = 채점을 확정할 수 없음(ERROR 등), 2 = 사용법·모드 오류. 스크립트에서 쓸 때 참고하세요([10장](#10-채점-규칙과-종료-코드)).

### 5.6 복습

```sh
ckad review                             # 결과와 실패 근거
ckad review --problem Q1 --solution     # 해설·모범 풀이 (열람 기록이 남습니다)
```

- 복습은 실패한 요구사항 → 실제 관찰 결과 → 진단 명령과 문서 → 원인 설명 → 모범 풀이 순입니다.
- 해설은 `--solution`으로 직접 열 때만 공개됩니다. 열람 이력은 재도전에서도 남습니다.
- 훈련에서는 문제별 최신 결과가 표시되고, 서로 다른 시점의 최고 점수를 합쳐 하나의 총점으로 만들지 않습니다. 훈련 전체의 총점이 필요하면 `ckad grade`를 새로 실행하세요.

### 5.7 다시 채점: `--retry`와 `--resubmit`

| 상황 | 명령 | 의미 |
| --- | --- | --- |
| ERROR가 났지만 답안은 안 고쳤다 | `ckad grade --retry` | 같은 제출을 **다시 관찰**합니다(기술적 재시도). |
| 답안을 고쳤다 | `ckad grade --resubmit` | **새 제출**로 기록합니다. 이전 제출·결과는 덮어쓰지 않습니다. |

답안이 바뀐 것이 확인되면 `--retry`는 거부되고 `--resubmit`을 안내합니다. 이미 제출한 세션에서 그냥 `ckad grade`를 다시 하면 "이미 제출했습니다. 기존 결과입니다..." 안내와 함께 기존 결과가 나옵니다.

### 5.8 문제 하나 초기화

```sh
ckad reset --problem Q1
```

진행 중인 훈련 문제 하나를 처음 상태로 되돌립니다. 이전 풀이는 `attempt-1`에 보존되고 새 `attempt-2` 폴더가 만들어집니다. 점수와 개인 설정은 그대로입니다. (모의시험 진행 중에는 거부됩니다.)

### 5.9 오답만 다시 풀기

훈련을 `ckad grade`로 전체 제출해 끝낸 뒤 진행합니다.

```sh
ckad status                       # 첫 줄의 세션 ID를 메모해 두세요 (재도전에 필요합니다)
ckad reset                        # 현재 세션을 끝내고 소유 리소스 정리 (환경 ID 입력 확인)
ckad start --retry <세션 ID> --failed
ckad start --retry <세션 ID> --failed --new-seed   # 검증된 범위의 새 조건
```

- `ckad reset`은 다음을 출력하고 **환경 ID**를 입력하라고 합니다. `ckad doctor` 첫 줄이나 이 안내에 나온 환경 ID를 그대로 입력하세요. 입력이 다르면 아무것도 지우지 않습니다.

  ```text
  세션 <세션 ID>를 포기하고 환경 <환경 ID>의 소유 리소스를 정리합니다. 풀이·성적·개인 설정은 보존됩니다.
  확인하려면 환경 ID를 입력하세요:
  ```

  완료되면 `초기화 완료. 이전 풀이와 학습 기록은 보존했습니다.`가 나옵니다.
- 재도전은 **채점이 끝난(COMPLETED) 또는 포기한(ABANDONED) 세션**에서만 가능하고, 1~20문제의 실패 항목이 있어야 합니다. ERROR만 난 문제는 약점으로 치지 않습니다. 원래 문제 패키지가 없어졌으면 다른 버전으로 대체하지 않고 거부합니다.

---

## 6. 모의시험

```sh
ckad start               # 기본 모의시험 mock-01
ckad start mock-01       # 같은 시험을 ID로 지정
```

`yes`로 시작을 확인하는 흐름은 연습 모드와 같습니다.

### 6.1 구성과 규칙

- **구성:** 20문제, 제한 시간 **120분**, 100점. 영역별 점수는 Design & Build 20, Deployment 20, Observability 15, Environment·Configuration·Security 25, Services & Networking 20이고 build·modify·troubleshoot·behavioral 유형을 모두 포함합니다. 문제 순서는 고정입니다([`exams/mock-01.yaml`](exams/mock-01.yaml)).
- **66% 기준은 참고용:** 채점 후 `참고 기준: 66% | 이번 비율: ...%`가 표시됩니다. 공식 합격 예측이 아닙니다.
- **문제별 채점·개별 초기화 불가:** 모의시험에서는 `ckad grade`로 전체를 한 번에 제출하고, 문제 하나만 `reset --problem` 할 수 없습니다.
- **해설은 제출 뒤에:** 모의시험의 해설은 전체 제출을 오류 없이 채점한 뒤에 열 수 있습니다.
- **시계:** 터미널을 닫거나 컴퓨터가 잠들어도 시간은 흐릅니다. 시간을 넘겨도 제출은 막지 않으며 초과 시간이 표시됩니다.

### 6.2 시험 끝낸 뒤: 채점 → 복습 → 반드시 `ckad reset`

```sh
ckad grade                        # 전체 제출 (여기서 시험이 끝납니다)
ckad review                       # 결과와 실패 근거
ckad review --problem Q4 --solution
ckad reset                        # 환경 ID를 입력해 정리
```

**끝낸 시험의 리소스는 `ckad reset`을 하기 전까지 클러스터에 남아 있습니다.** 정리하지 않고 다시 `ckad start`를 하면 거부됩니다(종료 코드 3).

```text
이전 학습 리소스가 남아 있습니다. ckad status 확인 후 ckad reset으로 정리하세요.
```

### 6.3 모의시험이 거부될 때

20문제 중 하나라도 출제할 수 없으면 **시험 전체를 거부**합니다(종료 코드 3, 세션을 만들지 않음). 다른 문제로 바꾸거나 점수를 재분배하지 않습니다. 화면에는 이런 일반 메시지만 나옵니다.

- `필요한 환경 capability가 준비되지 않았습니다. ckad doctor로 확인하세요.`
- `문제 검증 기록이 없거나 현재 환경과 다릅니다. ckad bank validate <problem-id>로 검증하세요.`

어느 문제가 왜 빠졌는지는 `ckad list`의 `출제 불가:` 줄과 `ckad doctor`로 확인합니다.

**`ckad doctor`에서 `network-policy`가 FAIL인 호스트**(차단을 실제로 적용하지 않는 환경)에서는 `networkpolicy-001`을 낼 수 없으므로 모의시험이 항상 거부됩니다. 이것은 의도된 동작입니다. 대처:

- **훈련 모드(`--mode practice`)** 를 쓰세요. 나머지 19문제는 출제됩니다.
- 모의시험이 꼭 필요하면 NetworkPolicy를 차단하는 호스트(차단을 적용하는 CNI)로 옮기세요. 그런 호스트에서는 `ckad bank validate networkpolicy-001`로 먼저 확인합니다.

---

## 7. 이미지 빌드 문제

`image-001`처럼 이미지를 만드는 문제는 관리 빌드 명령을 쓰면 reset 때 자동으로 정리됩니다.

```sh
ckad image build --problem Q1 --context . --tag junkloud.local/app:practice
docker save --output image.tar junkloud.local/app:practice   # 아카이브가 필요할 때
```

`--problem`은 Q 번호나 문제 ID(필수), `--tag`는 이미지 이름(필수), `--context`는 빌드 폴더(기본 `.`, 현재 attempt 폴더 기준)입니다. 성공하면 `이미지 빌드 완료: <이미지> (<ID>)`가 나옵니다.

- **빌드 조건:** 제한 시간 5분, 빌드 중 네트워크 없음. 베이스 이미지는 제공된 고정 이미지(또는 `scratch`)만 쓸 수 있습니다. Dockerfile은 `FROM` 한 단계와 로컬 `COPY`만 지원합니다.
- **일반 `docker build`도 채점됩니다.** 다만 소유가 기록되지 않아 reset이 자동으로 지우지 않고 소유권 오류로 알려 줍니다. 이 경우 **이미지를 직접 확인하고 삭제**해야 합니다. reset 전에 미리 지워 두어도 되고, reset이 소유권 오류로 멈췄다면 지운 뒤 `ckad reset --recover`를 실행합니다(`docker image rm <이미지>`).
- **모의시험을 채점한 뒤에는 `ckad image build`가 거부됩니다.** 현재 동작은 "세션이 진행 중(RUNNING)일 때만 관리 빌드 가능"입니다. 모의시험은 첫 `ckad grade`로 끝나므로, 채점한 뒤 복습하며 이미지를 다시 만들 때는 일반 `docker build`를 써야 하고(`image-001` 문제 문장도 이를 허용합니다) 그 이미지는 reset 전에 직접 지워야 합니다. 거부되면 종료 코드 2와 `현재 모드·상태에서 사용할 수 없는 요청입니다...`가 나옵니다.

자세한 규칙은 [`runtime/README.md`의 Image practice ownership](runtime/README.md#image-practice-ownership)을 참고하세요.

---

## 8. 자주 겪는 문제와 해결

명령은 모두 작업 컨테이너 안에서 실행합니다. 메시지는 `ckad`가 실제로 출력하는 문구입니다.

| 증상 | 원인과 해결 |
| --- | --- |
| `ckad start`가 종료 코드 3으로 거부됨 | 메시지에 따라 다릅니다. ① `이전 학습 리소스가 남아 있습니다...` → 이전 세션을 `ckad reset`으로 정리. ② `문제 검증 기록이 없거나 현재 환경과 다릅니다...` → `ckad bank validate <문제 ID>`. ③ `필요한 환경 capability가 준비되지 않았습니다...` → `ckad doctor`로 어느 기능이 FAIL인지 확인([3장](#3-환경-점검-ckad-doctor), [6.3](#63-모의시험이-거부될-때)). ④ `다른 준비·채점·검증 작업이 실행 중입니다...` → 다른 터미널의 작업이 끝나길 기다림. 어느 문제가 제외됐는지는 `ckad list`의 `출제 불가:` 줄에서 봅니다. |
| `진행 중인 학습이 있습니다. ckad status로 확인하고 학습을 마친 뒤 실행하세요.`(종료 코드 2) | 끝나지 않은 세션이 있습니다. 채점해서 끝내거나 `ckad reset`으로 포기하세요. |
| `출제 가능한 문제가 요청한 수보다 적습니다...`(종료 코드 2) | 훈련은 최소 5문제입니다. `--domain` 같은 필터를 줄이거나 검증된 문제를 늘리세요(`ckad list`로 확인). |
| `초기화가 중단되었습니다. 기존 풀이와 정리 기록을 보존했습니다. ckad status 확인 후 ckad reset --recover로 재개하세요.` | `reset`이나 이미지 빌드 중 끊겼습니다. `ckad status`가 미완료 작업을 보여 주면 `ckad reset --recover`로 이어서 정리합니다. 이 상태에서는 새 학습을 시작할 수 없습니다. |
| `초기화를 완료하지 못했습니다: ...` | 정리를 확인할 수 없는 PVC/PV, finalizer, 소유를 증명할 수 없는 이미지 등이 남았습니다. 메시지에 나온 대상을 확인(이미지면 직접 삭제)한 뒤 `ckad reset --recover`. finalizer를 무조건 제거하지는 않습니다. |
| 클러스터가 이상함(`kubectl`이 응답하지 않음, 객체가 사라짐 등) | 먼저 `ckad doctor`. 그래도 안 되면 `ckad reset --cluster`로 **전용 클러스터만** 다시 만듭니다(환경 ID 입력 확인). 풀이와 기록은 보존됩니다. 클러스터를 다시 만들기 때문에 진행 중이던 문제의 클러스터 리소스는 사라집니다. |
| 채점 결과가 **ERROR** | 판정할 수 없는 항목입니다(점수가 아님). `ckad doctor`로 클러스터와 기능 상태를 확인한 뒤, 답안을 고치지 않았다면 `ckad grade --retry`. 고쳤다면 `ckad grade --resubmit`. |
| 요구사항 이름이 `!inputs-changed`인 ERROR | 채점하는 동안 답안 대상이 바뀐 것으로 감지됐습니다. **채점 중에는 답안을 수정하지 마세요.** 건드리지 않았는데도 드물게 나타날 수 있습니다. HPA(자동 확장)가 있는 문제(`api-migration-001`)에서 채점 중 replicas가 바뀌면 그럴 수 있다고 의심하고 있으나 원인은 확정되지 않았습니다. `ckad grade --retry`가 거부되면(`답안이 바뀌었거나 동일함을 확인할 수 없습니다`) `ckad grade --resubmit`을 쓰세요. |
| `이미 제출했습니다. 기존 결과입니다...` | 이미 제출한 세션에서 `ckad grade`를 다시 실행했습니다. 기술적 오류는 `--retry`, 답안을 고친 뒤에는 `--resubmit`, 결과는 `ckad review`. |
| `모의시험은 문제별 채점을 지원하지 않습니다...`(종료 코드 2) | 모의시험은 `ckad grade`로 전체 제출만 가능합니다. |
| `모의시험의 해설은 전체 제출을 오류 없이 채점한 후 열 수 있습니다...` | 먼저 `ckad grade`. ERROR가 남아 있으면 `--retry`/`--resubmit`으로 해결해야 열립니다. |
| `현재 모드·상태에서 사용할 수 없는 요청입니다...`(종료 코드 2) | 명령이 지금의 모드·상태에 맞지 않습니다(예: 채점한 모의시험의 `ckad image build`, 진행 중 모의시험의 `reset --problem`). `ckad status`와 `--help`를 확인하세요. |
| Pod가 `OOMKilled`이거나 `Pending`(스케줄 실패), 클러스터 응답이 느려지거나 ERROR가 늘어남 | Docker VM 메모리가 부족할 수 있는 증상입니다. `docker info --format '{{.MemTotal}}'`로 VM 메모리를 확인하고, `kubectl get pods -A`, `kubectl describe pod <이름>`으로 원인을 봅니다. Docker Desktop의 메모리를 늘리거나 다른 컨테이너를 멈춘 뒤 `ckad reset --cluster`를 고려하세요. 메모리가 부족해도 JunKloud는 문제를 몰래 빼거나 점수를 재분배하지 않습니다. 4GiB에서의 동작은 아직 실기기에서 확인하지 않았습니다. |
| `kubectl`이 엉뚱한 클러스터에 접속 | 호스트 터미널이 아니라 **작업 컨테이너 안**에서 실행하는지 확인하세요. 컨테이너 안에서는 `KUBECONFIG`가 전용 파일(`$CKAD_STATE_DIR/environment/kubeconfig`)로 설정됩니다. 상태 경로를 바꿨다면 `export KUBECONFIG="$CKAD_STATE_DIR/environment/kubeconfig"`도 적용하세요. JunKloud는 호스트의 kubeconfig나 current-context를 바꾸지 않습니다. |
| `기록된 전용 환경 ID가 없습니다...` | 최초 환경 생성이 중단됐습니다. `ckad doctor`를 다시 실행하면 기록된 생성 작업을 재개합니다. |
| 시작 스크립트가 `kind` 네트워크 오류로 중단 | 기존 `kind` Docker 네트워크가 bridge가 아니면 안전을 위해 중단합니다. `docker network inspect kind`로 확인하세요. |
| 컨테이너 안에서 `docker`가 권한 오류 | 시작 스크립트가 Docker 소켓 권한을 맞추는 단계를 포함합니다. 저장소 루트에서 `bash runtime/up.sh`(Windows는 `runtime/up.ps1`)를 다시 실행해 컨테이너를 새로 띄워 보세요. |
| 모든 것을 처음부터 다시 | [2.6](#26-컨테이너-멈추기와-완전히-지우기) |

---

## 9. 명령어 요약표

아래는 `ckad --help`와 각 하위 명령의 `--help` 출력과 일치합니다. 모든 명령에 공통 옵션 `--bank-dir`, `--state-dir`, `--workspace`가 있습니다([11장](#11-경로와-환경-변수)).

| 명령 | 설명 (`--help`) | 주요 옵션 |
| --- | --- | --- |
| `ckad doctor` | 전용 환경·도구 version·플랫폼 capability 점검 | |
| `ckad list` | 현재 출제 가능한 문제·모의시험 조회 | |
| `ckad start [mock-id]` | 환경과 문제를 준비하고 확인 후 시작 | `--mode mock\|practice`(기본 `mock`), `--count`(훈련 문제 수 5~20, 기본 5), `--domain`, `--difficulty`, `--tag`, `--seed`, `--retry <session-id>`, `--failed`(오답만 재도전), `--new-seed`(검증 범위에서 변형 재도전) |
| `ckad status` | 학습 모드·목록·시간·복구 상태 조회 | |
| `ckad show <Q-number\|problem-id>` | 영어 문제·대상 context·namespace·제공 파일 조회 | |
| `ckad mark <Q-number\|problem-id>` | 다시 볼 문제 표시·해제 (채점하지 않음) | `--review`(표시), `--clear`(해제) |
| `ckad grade` | 전체 제출 또는 훈련 문제 채점 | `--problem`(훈련 문제 Q-number 또는 problem-id), `--retry`(수정하지 않은 오류 제출의 기술적 채점 재시도), `--resubmit`(수정 후 새 제출 기록) |
| `ckad review [session-id]` | 제출 결과·실패 근거 조회 (해설은 --solution) | `--problem`, `--solution`(해설·모범 풀이 명시 열람 및 이력 저장) |
| `ckad reset` | 소유 리소스를 정리하고 풀이·기록 보존 | `--problem`(훈련 문제 Q-number 또는 problem-id), `--cluster`(전용 클러스터 복구 재생성), `--recover`(기록된 미완료 reset 재개) |
| `ckad image build` | 현재 풀이의 Dockerfile로 소유권을 기록한 이미지 빌드 | `--problem`(필수), `--tag`(필수), `--context`(현재 attempt 아래의 빌드 context, 기본 `.`) |
| `ckad bank validate <problem-id>` | 초기·정답·대표 오답·reset과 parameter 범위 검증 | |

`ckad help`로 도움말을 볼 수 있고, `ckad <명령> --help`로 명령별 옵션을 확인하세요.

---

## 10. 채점 규칙과 종료 코드

| 판정 | 의미 | 점수 |
| --- | --- | --- |
| PASS | 요구사항 충족 | 해당 배점 획득 |
| FAIL | 풀이 결과가 요구사항을 충족하지 않음. 잘못된 이미지, selector, probe 때문에 앱이 실패한 경우도 포함. 컨트롤러가 학습자의 잘못된 spec(없는 ServiceAccount, 초과한 쿼터 등) 때문에 Pod를 만들지 못한 경우도 FAIL입니다(채점기 `grader-9`). | 0점과 실제 관찰 근거 |
| ERROR | 클러스터 API 불통, 검사 도구 준비 실패 등으로 판정할 수 없음 | 점수 확정 보류 |

- **부분 점수:** 항목마다 전부 아니면 0점이고, 문제 점수는 통과한 항목 배점의 합입니다.
- **ERROR가 있을 때:** 해당 제출은 미완료입니다. 확정 총점과 합격 여부를 표시하지 않습니다.
- **선행 항목:** 선행 항목이 실패하면 딸린 항목은 `prerequisite not satisfied`로 FAIL입니다.
- **의미 비교:** 예를 들어 `1` CPU와 `1000m`은 같은 값으로 봅니다. 문제가 요구하지 않는 한 YAML 키 순서, 문법, 임의의 라벨 이름으로 정답을 제한하지 않습니다.
- **숨은 조건 없음:** 채점 항목은 모두 영어 문제 문장에 적혀 있습니다. 명령 사용 이력(`kubectl top`을 실행했는지 등)은 채점하지 않습니다.
- **Secret 값:** 채점 결과·실패 메시지에 Secret 값을 출력하지 않습니다.
- **기다리는 시간:** 상태가 수렴할 때까지 기본 30초를 기다립니다. 네트워크 요청은 1회 3초, 최대 3회입니다. 더 긴 관찰이 필요한 문제는 문제에 그 한도가 적혀 있습니다.
- **모의시험 66%:** 공개된 합격 기준을 참고용으로만 표시합니다. 공식 합격 예측이 아닙니다.

`ckad` 종료 코드:

| 코드 | 의미 |
| --- | --- |
| 0 | 조작 성공 또는 만점 |
| 1 | 정상 채점이지만 요구사항 미충족 |
| 2 | 사용법·모드 오류 |
| 3 | 환경·저장·채점 불가 |

### 초기화(reset)와 복구 요약

```sh
ckad reset --problem Q1   # 훈련 문제 하나 초기화 → 새 attempt 폴더 생성
ckad reset                # 세션 포기 + 소유 리소스 전체 정리 (환경 ID 입력으로 확인)
ckad reset --cluster      # 전용 클러스터 재생성 (환경 ID 입력으로 확인)
ckad reset --recover      # 중단된 초기화·이미지 빌드 기록을 이어서 정리
```

- **보존되는 것:** 이전 풀이 폴더, 점수, 개인 설정은 지우지 않습니다. 정리 대상은 JunKloud가 만들고 소유를 기록한 리소스뿐입니다.
- **건드리지 않는 것:** 다른 Docker 리소스와 소유하지 않은 클러스터 객체는 변경하거나 삭제하지 않습니다. 전역 `prune`도 쓰지 않습니다.
- **모의시험 중:** 진행 중인 모의시험의 문제 하나만 초기화하는 것은 거부합니다.
- **정리 실패:** 정리를 확인할 수 없는 PVC/PV나 finalizer가 있으면 오류와 기록을 남기고, 정리가 끝나지 않은 동안에는 새 학습을 시작하지 않습니다.
- **중단 복구:** 프로세스가 중단되면 다음 명령이 미완료 작업을 감지하고 복구 방법을 안내합니다. 사라진 namespace나 클러스터를 답안과 함께 자동으로 새로 만들지 않습니다.

---

## 11. 경로와 환경 변수

| 용도 | 기본값 | 환경 변수 | 옵션 |
| --- | --- | --- | --- |
| 문제은행 루트(`problems/`, `exams/`) | `/src` | `CKAD_BANK_DIR` | `--bank-dir` |
| 상태(세션 DB, kubeconfig, 기록) | `$HOME/.local/share/junkloud` | `CKAD_STATE_DIR` | `--state-dir` |
| 풀이 작업 폴더 | `$HOME/ckad` | `CKAD_WORKSPACE` | `--workspace` |

- 풀이 폴더 구조는 `<workspace>/<session>/<problem>/attempt-<n>/`입니다. 문제를 초기화하면 다음 번호의 attempt 폴더가 생기고, 이전 attempt는 그대로 남습니다.
- 개발용 fixture 은행: `CKAD_BANK_DIR=/src/tests/fixtures/dev-bank`. fixture의 `dev-mock`은 제품 모의시험이 아닙니다.

---

## 12. 문제은행

| ID | 영역 | 유형 | 난이도 | 점수 | 주제 |
| --- | --- | --- | --- | ---: | --- |
| `image-001` | Design & Build | build | medium | 4 | 명령을 실행 가능한 이미지로 패키징 |
| `batch-001` | Design & Build | build | medium | 4 | Job 실행과 CronJob 일정 |
| `daemonset-001` | Design & Build | build | medium | 4 | DaemonSet으로 노드 에이전트 배치 |
| `multicontainer-001` | Design & Build | build | medium | 4 | init·sidecar 컨테이너와 임시 볼륨 |
| `volume-001` | Design & Build | modify | medium | 4 | Pod를 교체해도 데이터 유지(PVC) |
| `rollout-001` | Deployment | troubleshoot | medium | 5 | 멈춘 rollout 복구 |
| `release-001` | Deployment | build | medium | 5 | stable·canary 백엔드 구성 |
| `helm-001` | Deployment | build | medium | 5 | 로컬 Helm chart와 release values |
| `kustomize-001` | Deployment | build | medium | 5 | base를 건드리지 않는 overlay 배포 |
| `api-migration-001` | Observability | modify | medium | 5 | 폐기된 API manifest를 제공되는 API로 이전 |
| `probes-001` | Observability | troubleshoot | medium | 5 | readiness·liveness·startup probe 수정 |
| `diagnostics-001` | Observability | troubleshoot | medium | 5 | CrashLoop 원인 진단과 CPU 사용량 확인(`kubectl top`) |
| `custom-resource-001` | Environment·Configuration·Security | build | easy | 5 | CRD에서 Backup 리소스 종류를 찾아 사용자 리소스(CR) 생성 |
| `access-001` | Environment·Configuration·Security | modify | medium | 5 | ServiceAccount와 네임스페이스 RBAC로 최소 권한 부여 |
| `resources-001` | Environment·Configuration·Security | troubleshoot | medium | 5 | ResourceQuota·LimitRange에 맞춘 requests·limits 수정 |
| `configuration-001` | Environment·Configuration·Security | troubleshoot | easy | 5 | 잘못된 환경 변수를 ConfigMap·Secret 참조로 교체(Secret 값은 결과에 노출되지 않음) |
| `security-001` | Environment·Configuration·Security | behavioral | hard | 5 | `restricted` Pod Security 정책을 지키면서 워크로드 실행(securityContext) |
| `service-001` | Services & Networking | troubleshoot | medium | 6 | 끊어진 Service(selector, targetPort, NodePort) 복구 |
| `ingress-001` | Services & Networking | build | medium | 6 | host·path 규칙으로 두 Service에 요청을 나누는 Ingress |
| `networkpolicy-001` | Services & Networking | behavioral | hard | 8 | api로 들어오는 연결과 web에서 나가는 연결만 허용하는 NetworkPolicy |

- **영역별 합계:** Design & Build 5문제/20점, Deployment 4문제/20점, Observability 3문제/15점, Environment·Configuration·Security 5문제/25점, Services & Networking 3문제/20점으로 합계 20문제/100점입니다. 유형은 build·modify·troubleshoot·behavioral을 모두 포함합니다.
- **영역 매핑:** [`curriculum/coverage.yaml`](curriculum/coverage.yaml)에 있습니다.
- **필요한 기능(capability):** `image-001`은 `image-build`, `volume-001`은 `storage`, `diagnostics-001`은 `metrics`, `ingress-001`은 `ingress`, `networkpolicy-001`은 `network-policy`가 필요합니다. 환경이 이 기능을 지원하지 않으면 해당 문제는 출제하지 않고, 모의시험은 시작하지 않습니다.
- **변형:** 문제마다 parameter(namespace 등)가 seed에 따라 바뀝니다.

문제 패키지 구조, 작성 규칙, 검증 절차는 [문제 작성 안내](docs/authoring/problem-guide.md)에 있습니다. Helm·Kustomize 작업 규칙은 [`docs/author-tool-actions.md`](docs/author-tool-actions.md), 스키마는 [`schemas/problem-v1.schema.json`](schemas/problem-v1.schema.json)입니다.

---

## 13. 개발자용·검증용

개발 컨테이너에는 Go 컴파일러와 race 검사용 C 도구가 추가로 들어 있습니다.

```sh
bash runtime/up.sh dev                       # Windows: runtime/up.ps1 -Service dev
docker compose exec dev bash
```

컨테이너 안에서:

| 명령 | 하는 일 |
| --- | --- |
| `make test` | `go test ./...` 단위 테스트 |
| `make test-race` | `go test -race ./...` |
| `make lint` | 모든 빌드 태그(`integration`, `e2e`, `acceptance`)의 `go vet`과 `gofmt` 검사 |
| `make test-runtime` | 작업 환경 자체 검증(Linux, 볼륨, 고정 도구 버전) |
| `make test-integration` | 실제 kind 클러스터를 만들고 지우는 통합 테스트 (매우 오래 걸림) |
| `make test-e2e` | `ckad` CLI E2E(`TestCLIFlow` 등). 시간 상한은 Makefile 참고 |
| `make test-acceptance` | 릴리스 수용 시험: 준비 → 모범 풀이 → 채점 → reset을 3회 연속 수행(수 시간) |

문제은행 통합 테스트(실제 클러스터 검증) 예:

```sh
go test -tags=integration ./tests/integration -run '^TestObservabilityBankSmoke$' -count=1 -timeout=30m -v
go test -tags=integration ./tests/integration -run '^TestObservabilityBank$' -count=1 -timeout=150m -v
# 한 문제만: -run '^TestObservabilityBank$/^probes-001$'
```

다른 문제은행 테스트(`TestBuildBank`, `TestDeploymentBank`, `TestSecurityBank`, `TestNetworkBank` 등)도 같은 방식으로 실행합니다.

호스트가 표준과 다를 때 쓰는 테스트 전용 환경 변수입니다. 제품 기본값을 바꾸지 않습니다. 실제 기기(Mac/Windows Docker Desktop)에서는 보통 설정하지 않습니다.

| 변수 | 쓰는 경우 |
| --- | --- |
| `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1` | cgroup v1 호스트, `oom_score_adj` 조정이 막힌 VM |
| `JUNKLOUD_TEST_HOST_COMPAT=cgroupv1,no-ipv6` | 위 조건에 더해 엔진 커널의 IPv6가 꺼져 있을 때 |
| `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES=network-policy` | 커널이 NetworkPolicy 차단을 지원하지 않음을 미리 알 때. doctor가 FAIL로 보고하는지 확인하고, 해당 시험은 `PARTIAL: mock-01 minus [...]`로 표시되며 mock-01로 보고하지 않습니다. |
| `JUNKLOUD_ACCEPTANCE_EVIDENCE=<폴더>` | `make test-acceptance`의 사이클별 JSON 증거를 보관 |
| `JUNKLOUD_ACCEPTANCE_COLD=1` | 첫 시작을 cold start로 표시하도록 요청(이미지가 이미 있으면 "cold (unverified)"로 기록) |

- E2E와 통합 테스트는 실제 Docker, kind, kubectl과 클러스터 자원이 필요합니다. 사용할 수 없으면 실패를 반환합니다. 건너뛴 검증을 통과로 표시하지 않습니다.

### 실기기 검증 절차 (`runtime/acceptance.sh`)

Mac/Windows 실기기에서 수용 시험을 돌리는 절차입니다. **이 시험은 아직 실기기에서 실행하지 않았습니다.**

```sh
bash runtime/up.sh dev                  # Windows: runtime/up.ps1 -Service dev
docker compose exec dev bash
bash runtime/acceptance.sh              # 수 시간. 끝나면 요약표가 출력됩니다
```

스크립트는 호스트 정보(`uname -m`, Docker 메모리·CPU·버전)를 출력하고, 임시 상태 폴더에서 `ckad doctor`와 문제 20개의 `ckad bank validate`를 시간과 함께 실행한 뒤(내 학습 상태는 건드리지 않음) `make test-acceptance`를 돌립니다. `ckad doctor`에서 FAIL인 기능은 자동으로 `JUNKLOUD_TEST_UNSUPPORTED_CAPABILITIES`에 전달되어 **PARTIAL** 시험으로 실행되고 요약에 그렇게 표시됩니다. 요약표와 `$HOME/junkloud-acceptance` 안의 `acceptance.log`, `cycle-*.json`을 보내 주세요. 자세한 내용과 결과 칸은 [수용 시험 매트릭스](docs/validation/acceptance-matrix.md)에 있습니다.

### CI

[`.github/workflows/ci.yaml`](.github/workflows/ci.yaml)은 GitHub의 네이티브 러너(`ubuntu-24.04`, `ubuntu-24.04-arm`)에서만 실행하며 QEMU 같은 에뮬레이션은 쓰지 않습니다. `static`(단위 테스트, `make lint`, amd64·arm64 교차 빌드)과 `image`(워크스페이스·개발 이미지 빌드, 두 아키텍처)는 push와 pull request마다, `integration-smoke`와 `validate-networkpolicy`는 수동 실행(`workflow_dispatch`)에서만 돕니다. 이 워크플로는 정의만 있고 GitHub에서 실행한 결과는 아직 없습니다.

---

## 14. 현재 상태와 제한

- **채점기 버전:** `grader-9`. 컨트롤러가 학습자의 잘못된 spec 때문에 Pod를 만들지 못한 경우(없는 ServiceAccount, 초과한 쿼터)를 ERROR가 아니라 FAIL로 채점합니다. 오래된 실패 조건이 남은 경우(ServiceAccount를 뒤늦게 만든 직후 등)는 FAIL이 아니라 ERROR로 안내합니다. 채점기 버전이 바뀌면 이전 검증 기록은 무효입니다.
- **문제은행:** 20문제 / 100점이고 기본 모의시험 `mock-01`이 있습니다. 구성(20문제, 120분, 100점, 영역별 점수)은 테스트로 고정돼 있습니다.
- **검증한 환경과 증거:** 개발 샌드박스(Linux amd64, cgroup v1)입니다. 이 샌드박스의 커널은 **NetworkPolicy를 적용하지 않습니다.** 샌드박스에서 수행한 수명주기·복구 시험의 결과와 한계는 [수용 시험 매트릭스](docs/validation/acceptance-matrix.md)에 기록합니다. 거기서 "대기"나 "미완료"로 적힌 칸은 아직 확인되지 않은 것입니다. 샌드박스 결과는 Apple M2나 Windows 칸을 채우지 않습니다.
- **수명주기 시험은 PARTIAL입니다.** 샌드박스에서 돌린 3회 연속 수명주기 시험은 `networkpolicy-001`을 뺀 19문제(92점)로 실행했고, `PARTIAL: mock-01 minus [networkpolicy-001]`로 표시하며 mock-01 결과로 보고하지 않습니다. 이 시험이 쓰는 검증 기록은 임시 대체물(stand-in)이라 문제 패키지 검증 증거가 아닙니다.
- **`networkpolicy-001`은 NOT CERTIFIED입니다.** 정답·대표 오답 점수는 설계값이며, 차단하는 호스트에서 `ckad bank validate`로 확인한 기록이 아직 없습니다. 그래서 **mock-01을 실제로 끝까지 시작·채점한 실행도 NOT RUN**입니다(샌드박스에서는 모의시험이 거부되는 것까지만 확인했습니다).
- **실기기 검증은 미완료입니다.** Apple M2(ARM64) 8GB에 4GiB Docker VM, Windows 16GB에서의 실행, cold/warm 시작·reset·채점 시간, 4GiB 적합 여부는 아직 측정하지 않았습니다. 4GiB는 목표 예산이며 실행 보장이 아닙니다. "출시 준비 완료"는 실기기 검증이 끝난 뒤에만 선언합니다.
- **알려진 동작과 제한:**
  - 모의시험을 채점한 뒤에는 `ckad image build`가 거부됩니다([7장](#7-이미지-빌드-문제)).
  - 이미 존재하지만 느슨해진 ResourceQuota 등, 오래된 실패 조건이 아직 있는 경우는 감지하지 못합니다.
  - 채점 중 답안 대상이 바뀐 것으로 감지되는 드문 ERROR(`!inputs-changed`)의 원인이 확정되지 않았습니다([8장](#8-자주-겪는-문제와-해결)).
  - 문제별·전체 reset과 전체 채점의 소요 시간은 샌드박스 측정만 있고 실기기 시간은 없습니다. 특정 초 단위 속도는 보장하지 않습니다([수용 시험 매트릭스](docs/validation/acceptance-matrix.md)).
- **작업 컨테이너 도구:** 설계상 제공할 도구 중 `wget`과 man pages는 아직 이미지에 없습니다(`curl`은 있습니다).
- **범위 밖:** 실제 시험의 SSH 진입·복귀, 다중 클러스터 전환, 시험 감독, 브라우저 제한은 재현하지 않습니다.
- **비공식:** 점수와 부분 점수 방식은 JunKloud 자체 규칙입니다. 공식 채점 방식과 같다고 주장하지 않으며, 문제는 독창적으로 작성했습니다.

---

## 15. 관련 문서

- [macOS 사용 안내](docs/usage/mac.md) · [Windows 사용 안내](docs/usage/windows.md): Docker Desktop 설정, 시작 스크립트, 두 번째 터미널, `k` 별칭, Docker 소켓 권한 범위
- [훈련 흐름 안내](docs/usage/training.md): 연습·모의시험·채점·복습·reset·오답 재도전·오류 복구
- [문제 작성 안내](docs/authoring/problem-guide.md): 패키지 구조, 작성 규칙, `ckad bank validate` 주기
- [수용 시험 매트릭스](docs/validation/acceptance-matrix.md): 시험 항목별 증거와 기기 실행 절차
- [작업 환경 시작 안내](runtime/README.md): 시작 스크립트, 학습 명령, 이미지 실습 소유권
- [설계 문서](docs/superpowers/specs/2026-10-14-ckad-training-system-design.md): 요구사항, 채점 계약, 세션·공개 규칙
- [구현 계획](docs/superpowers/plans/2026-10-14-ckad-training-system-implementation.md): Task별 진행 기록
- [Helm·Kustomize 작성자 동작](docs/author-tool-actions.md)
