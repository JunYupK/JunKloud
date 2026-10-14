# JunKloud — 로컬 CKAD 실전 훈련 시스템 설계

- 작성일: 2026-10-14 (Asia/Seoul)
- 상태: **사용자 승인 완료** — 2026-10-14, 구현 계획 작성 요청으로 written spec 승인 확인.
- 구현 승인 상태: 미승인. implementation plan을 별도로 작성·검토하고 실행 방식을 선택한다.
- 대상 저장소: https://github.com/JunYupK/JunKloud
- 설계 원칙: 실제 CKAD 학습 효율, 신뢰할 수 있는 채점, 반복 가능한 환경, 불필요한 기능 최소화.

## 1. 목적과 성공 조건

JunKloud는 개인이 실제 Kubernetes 클러스터에서 CKAD 문제를 풀고, 실제 상태와 동작에 근거한 자동 채점을 받고, 오답을 다시 연습하는 로컬 CLI 애플리케이션이다.

핵심 경험은 `ckad start` → 환경과 문제 준비 → 일반 터미널에서 `kubectl`로 풀이 → `ckad grade` → 결과와 해설 확인 → 초기화·재도전이다. 일반 세트는 5~20문제를 제공한다. 기본 모의시험은 20문제, 120분, 100점이다. 짧은 세션은 훈련 모드로 제공하며, 오답 재도전은 한 문제만 남아 있어도 가능하다.

사용자는 한 달 동안 CKAD를 준비하지만, 개발 기간을 며칠로 제한하지 않는다. 개발 중에는 다른 실습을 병행한다. 따라서 급하게 기능을 줄이는 대신 채점 정확성, 문제 품질, 초기화 안정성을 우선한다. 이것이 웹·계정·복잡한 플랫폼 기능을 추가할 이유는 아니다.

성공은 다음 조건을 모두 만족하는 것이다.

1. 환경 구동 후 Linux 셸 안에서 모든 학습 명령을 동일하게 사용한다.
2. 문제 준비부터 제출·복습·재도전까지 수작업으로 클러스터를 복구할 필요 없이 반복한다.
3. 요구사항을 충족하는 다양한 풀이를 인정하고, 대표적인 잘못된 풀이를 정확히 구분한다.
4. M2 MacBook 8GB와 Windows 16GB에서 지원 아키텍처에 맞춰 안정적으로 실행된다.
5. 기존 Kubernetes 환경과 관련 없는 Docker 리소스를 프로그램이 변경·삭제하지 않는다.
6. 공식 curriculum의 역량별로 검증된 자체 문제를 제공한다.

## 2. 확정한 사용자 경험과 범위

| 항목 | 결정 |
| --- | --- |
| 실행 환경 | Mac·Windows는 호스트, `ckad`·셸·`kubectl`·채점은 공통 Linux 작업 환경에서 실행 |
| 최소 사양 기준 | Apple M2 / 메모리 8GB. Windows는 16GB이며 CPU 아키텍처는 구현 검증 시 확인 |
| 인터페이스 | 일반 터미널. 웹 UI와 브라우저 내 터미널 없음 |
| 언어 | 문제·요구사항은 영어, 해설·오답 피드백은 한국어 |
| 제한시간 | 자율 마감. 초과 후에도 풀이 가능, 수동 제출 시 소요·초과 시간 기록 |
| 모의시험 | 전체 제출 후 점수·세부 채점·해설 공개 |
| 훈련 | 문제별 즉시 채점과 해설 열람 가능. 열람 여부 기록 |
| 개인 설정 | Bash alias, 자동완성, 편집기 설정 및 풀이 파일 영속 보관 |
| SSH | 진입·복귀 연습은 MVP에서 제외 |
| context | 전용 클러스터 하나와 명시된 context 사용. 실제 다중 클러스터 전환은 Post-MVP |
| AI | 핵심 실행과 채점에는 AI·외부 LLM API가 필요하지 않음 |

MVP에서 하지 않는 것: SaaS, 사용자 계정, 다중 사용자, 클라우드 배포, 결제, leaderboard, 자체 LLM, OpenAI/Anthropic API 연동, runtime AI 출제, 복잡한 analytics, adaptive sampling, spaced repetition, 독점 시험 UI 복제, 시험 감시·부정행위 방지 시스템.

개인 연습용 프로그램이므로 로컬 소유자가 소스를 직접 열어보는 행위까지 막지 않는다. 대신 정상적인 시험 사용 경로에서는 해설·모범 답안·채점 내부 정보가 제출 전에 노출되지 않도록 한다.

## 3. 공식 시험 기준과 자료 사용

### 3.1 확인 기준

2026-10-14에 조회한 Linux Foundation FAQ와 Important Instructions는 CKAD 시험 환경을 Kubernetes **v1.37**, Linux 명령줄 기반 수행형 시험, **15~20개 과제·120분**으로 안내한다. 상품 안내 페이지에는 v1.35 표기도 남아 있어 출처 간 차이가 있다. 실제 시험 환경을 직접 설명하는 FAQ와 Important Instructions를 버전 기준으로 삼는다. [S1][S2][S4]

CNCF의 공식 curriculum 저장소에는 v1.37 PDF가 게시되어 있다. 초기 웹 조사에서는 PDF 본문 가져오기가 실패했으나, 2026-10-14 구현 계획 작성 과정에서 공식 원본을 직접 읽고 2페이지를 렌더링해 영역 배치까지 확인했다. 아래 역량 범위와 공식 배점이 일치하며 Kustomize는 Application Deployment에 포함된다. 확인한 PDF의 SHA-256은 `7736d2f0291239d62e9fc618304413ee9b6e53fc4580aac70bf4328203c1c210`이다. [S3][S4]

| 영역 | 모의시험 배점 | 포함할 학습 역량 |
| --- | ---: | --- |
| Application Design and Build | 20 | 컨테이너 이미지 정의·빌드·수정, 적합한 workload 선택, multi-container/init/sidecar 패턴, 영속·임시 볼륨 |
| Application Deployment | 20 | blue/green·canary 등 배포 전략, rolling update·rollback, 기존 Helm 패키지 배포, Kustomize |
| Application Observability and Maintenance | 15 | API deprecation 대응, probes·health checks, CLI 모니터링, 로그 활용, 애플리케이션 디버깅 |
| Application Environment, Configuration and Security | 25 | CRD·Operator가 제공하는 리소스의 발견·사용, 인증·인가·admission, requests·limits·quotas, ConfigMap, Secret, ServiceAccount, SecurityContext·capabilities |
| Services and Networking | 20 | NetworkPolicy, Service 접근 구성·장애 해결, Ingress |

역량별 문제 매핑을 관리한다. 서로 중복되는 공식 항목은 매핑에서 함께 표시할 수 있지만, 임의로 공식 범위를 삭제하지 않는다. CRD·Operator는 제공된 기능의 활용을 연습하며, Operator 프레임워크 구현이나 클러스터 관리자 과정 전체를 추가하지 않는다.

### 3.2 공개 시험 규칙의 반영과 차이

- 공식 허용 자료에는 Kubernetes Documentation·Blog, Helm Documentation 및 문제별 Quick Reference 자료가 있다. Kubernetes 문서 내 검색은 허용되지만 외부 검색 결과로 나가서는 안 된다고 안내한다. 문제에는 필요한 공식 문서 링크를 제공하고, 해설에는 관련 문서의 구체적인 항목을 연결한다. [S5]
- 공개 지침은 지정 호스트로 SSH 접속하고 종료 후 복귀하는 흐름을 안내한다. 사용자의 결정에 따라 이 동작은 재현하지 않는다. [S2]
- 공개 지침의 기본 도구에는 `kubectl`, `k` alias, Bash 자동완성, `yq`, `curl`, `wget`, man pages가 있다. 공통 작업 환경에 이를 제공한다. 개인 alias·편집기 설정을 추가할 수 있다. 호스트 터미널의 단축키까지 실제 시험 원격 데스크톱과 같다고 보장하지 않는다. [S2]
- 실제 시험의 감독·브라우저·외부 자료 제한을 기술적으로 강제하지 않는다. 사용자 선택인 시간 초과 풀이도 실제 제한시간과의 차이로 표시한다. 합격점으로 안내되는 66%는 참고 기준으로만 표시하며 로컬 점수를 공식 합격 예측으로 표현하지 않는다. [S1][S6]
- 자체 부분 점수와 세부 검사 방식은 JunKloud의 공개 규칙이다. 비공개 공식 채점 방식과 같다고 주장하지 않는다.
- 문제는 공식 curriculum, Kubernetes 문서와 일반적인 운영 패턴에서 독창적으로 작성한다. 실제 비공개 시험 문제, NDA 위반 자료, KodeKloud·Killer.sh 등 유료 문제의 복제본은 사용하지 않는다.

## 4. 실행 환경과 클러스터

### 4.1 선택한 구조

호스트의 Docker Desktop Linux 컨테이너 환경에서 다음을 구동한다.

1. **Linux 작업 컨테이너:** Bash, `ckad`, `kubectl`, Helm, `yq`, 편집기, HTTP 도구, Docker CLI, kind 및 필요한 검사 도구.
2. **kind 노드 컨테이너:** control-plane과 workload를 함께 실행하는 단일 노드 클러스터.
3. **학습에 필요한 클러스터 구성요소:** DNS, NetworkPolicy enforcement, Ingress, 저장소 제공 기능, CLI 리소스 모니터링에 필요한 metrics 서비스.

작업 컨테이너와 kind 노드는 같은 Docker 엔진의 별도 컨테이너다. 작업 컨테이너 안에 추가 Docker daemon을 중첩 설치하지 않는다. 호스트에는 환경 시작·접속을 위한 얇은 bootstrap만 제공하고, `ckad` 로직을 Mac·Windows용으로 따로 구현하지 않는다.

기본 접속은 `docker compose exec workspace bash`와 같은 일반 터미널 접속이다. 여러 터미널에서 동일한 작업 컨테이너에 접속할 수 있다. 이것은 SSH 훈련을 의미하지 않는다.

작업 컨테이너에서 클러스터 API에 접근할 때는 Docker 내부 네트워크에서 도달 가능한 endpoint를 사용한다. 호스트의 localhost 주소를 컨테이너의 localhost로 오인하지 않는다. endpoint에 맞는 TLS 검증을 유지하며 `insecure-skip-tls-verify`로 문제를 숨기지 않는다.

### 4.2 선택 근거와 대안

| 후보 | 장점 | 이번 선택에서의 판단 |
| --- | --- | --- |
| kind | upstream Kubernetes 버전과 노드 이미지를 명시적으로 고정, 동일한 환경으로 문제 검증 | 채택 |
| k3d/k3s | 경량 Kubernetes, NetworkPolicy 컨트롤러와 Traefik 기본 제공 | 메모리 검증에서 kind가 부적합할 경우 재검토할 대안. MVP에서 두 backend를 동시에 구현하지 않음 |
| minikube | 다양한 드라이버와 애드온 | 공통 컨테이너 기반 제품에서는 선택·관리할 조합이 늘어 우선순위 낮음 |

kind는 v0.24부터 기본 NetworkPolicy 지원을 추가했다. 별도 Calico/Cilium을 무조건 설치하지 않고 기본 제공 기능을 검증한다. 조회한 v0.33 릴리스는 Kubernetes v1.37.0 및 ARM64·AMD64 노드 이미지를 제공한다. 초기 검증 기준은 **kind v0.33 / Kubernetes v1.37**로 한다. 실제 배포 아티팩트의 정확한 patch와 digest는 구현 검증에서 확인해 버전 manifest에 고정한다. `latest`에 의존하지 않는다. [S7][S8]

Ingress 구성은 kind 공식 가이드의 `cloud-provider-kind` v0.9+ 지원 경로를 우선 검증한다. 추가 프로세스와 수명 주기도 관리 대상이다. Controller의 존재뿐 아니라 host/path 라우팅, 애플리케이션 응답, 잘못된 라우팅의 실패를 테스트한다. 이 경로가 ARM64·자원·기능 검증을 통과하지 못하면 검증 결과와 대체 구성을 설계 변경으로 기록한다. [S9]

### 4.3 아키텍처·메모리·영속성

- M2에서는 Linux ARM64를 네이티브 실행한다. Windows는 확인된 CPU에 맞춘 Linux 아키텍처를 사용한다. 도구의 동작과 버전을 통일하며 서로 다른 CPU용 바이너리를 하나로 강제하지 않는다.
- 문제에 사용하는 모든 이미지와 부가 구성요소의 지원 아키텍처를 검사한다. ARM64 미지원 이미지를 자동 에뮬레이션으로 숨기지 않는다.
- 초기 검증 자원 예산은 Docker Linux VM 메모리 **4GiB**로 잡는다. 이는 M2 8GB에서 검증할 목표이며 현재 실측된 소비량이나 실행 보장이 아니다.
- 초기 이미지 다운로드·문제 준비는 타이머 시작 전에 끝낸다. 모의시험의 모든 문제를 준비해 자유로운 문제 이동을 지원한다. 메모리 부족 시 문제를 몰래 생략하거나 점수를 재분배하지 않는다.
- workload는 개념을 검증하는 데 필요한 최소 규모로 구성한다. replicas·limits 등 문제 자체가 요구하는 값은 임의로 낮추지 않는다. 전체 세트가 예산을 초과하면 문제 세트와 fixture를 조정해 재검증한다.
- 작업 홈, 풀이, 결과 DB는 Linux named volume에 보관한다. Windows/macOS 파일 권한 차이에 세션 저장을 의존하지 않는다.
- `.bashrc`, 편집기 설정, 개인 풀이 파일은 문제 reset과 독립적이다. 과거 풀이를 새 시험 폴더에 자동으로 섞지 않는다.

## 5. 애플리케이션 구조와 기술 선택

### 5.1 Go CLI + Kubernetes API + SQLite

| 결정 | 이유와 대안 |
| --- | --- |
| Go | Kubernetes 객체·오류·취소·시간 제한을 명시적으로 다루고 실행 파일로 배포. Python도 공식 클라이언트를 갖춘 유효한 대안이지만 이번에는 Go로 통일 |
| Kubernetes API 중심 | `client-go`로 설정·상태·로그·exec 등 필요한 기능 접근. `kubectl`의 사람이 읽는 표나 오류 문자열을 채점 근거로 파싱하지 않음 |
| 제한된 subprocess | kind, Helm, 이미지 빌드·검사 등 실제 외부 도구의 의미가 필요한 작업에 사용. shell 문자열 결합 대신 인자 배열·시간 제한·종료 코드 처리 |
| SQLite | 세션·제출·검사 결과를 로컬 트랜잭션으로 저장. 별도 서버 없음. JSON 파일만 사용할 때 필요한 다중 파일 갱신 규칙을 줄임 |
| YAML + Markdown | 문제 구조·검사 기준은 YAML, 긴 문제·해설은 Markdown. schema로 검증 |

Go와 Python의 공식 클라이언트 존재, client-go 버전 호환 정책, SQLite의 로컬 애플리케이션 저장 용도는 각각 공식 자료를 참고했다. 라이브러리 버전도 선택한 Kubernetes 버전과 함께 검증·고정한다. [S12][S13][S14]

### 5.2 컴포넌트 경계

| 컴포넌트 | 책임 | 직접 다루지 않는 것 |
| --- | --- | --- |
| CLI | 명령 입력, 영어 문제 표시, 한국어 피드백, 사용자 오류 안내 | Kubernetes 검사 규칙의 구현 |
| Problem Bank | schema 검증, 문제 조회·선택, 변수 확정, 검증 결과 확인 | 진행 중인 세션 변경 |
| Session | 모드·타이머·제출·재도전·공개 시점 관리 | 개별 리소스 판정 로직 |
| Environment | 전용 클러스터 식별, setup, probe 실행 기반, 소유 리소스 정리 | 점수 정책 |
| Grader | 검사 계획 실행, 증거·판정·부분 점수 생성 | AI 평가, CLI 표시 방식 |
| Store | 세션·제출·항목별 결과의 트랜잭션과 조회 | 외부 클러스터의 롤백 |

하나의 CLI 코드베이스로 구성한다. 별도의 애플리케이션 서버, 메시지 브로커, 일반화된 plugin 플랫폼은 만들지 않는다. 모듈 경계는 함수·인터페이스 수준이며 서비스 분리를 뜻하지 않는다. Problem Authoring과 Exam Runtime은 동일한 schema·grader를 사용하지만 실행 경로와 작업 대상은 분리한다.

## 6. 문제 정의와 재현성

### 6.1 파일 구성

```text
problems/<problem-id>/
  problem.yaml
  question.en.md
  setup/
  solution/
  explanation.ko.md
  cases/
```

- `problem.yaml`: schema version, ID, 문제 version, title, 주 영역, 역량 매핑, difficulty, 유형·tags, parameter 정의, context·namespace, 초기 상태·준비 조건, grading criteria, 점수, 요구 capability, resource budget, 공식 출처.
- `question.en.md`: 영어 과제, 배점, 요구사항, 제공 파일과 산출물 위치. 사용자에게 요구하지 않은 조건을 숨은 채점 기준으로 추가하지 않는다.
- `setup/`: 초기 Kubernetes 리소스와 사용자에게 제공할 파일·로컬 Helm chart·빌드 context.
- `solution/`: 모범 풀이의 재현 가능한 작업 순서와 리소스. 단순 apply만으로 표현할 수 없는 Helm·이미지 작업은 검증된 작업 종류와 인자로 표현한다.
- `explanation.ko.md`: 핵심 개념, 실패 원인 진단 순서, 풀이 설명과 공식 문서.
- `cases/`: 대표적인 잘못된 풀이와 예상 검사 결과. 동등한 대체 정답 사례도 필요한 문제에 포함한다.

별도 metadata 파일로 같은 값을 중복 저장하지 않는다. 문제 스키마와 내용 버전은 구분한다. MVP의 schema version은 `1`이다. 정의 파일은 알려진 검사·작업 종류만 허용하고, 임의 코드 평가나 YAML 안의 일반 shell 실행 기능은 제공하지 않는다.

### 6.2 검증 규칙

- 필수 필드, 알려진 영역·검사 종류, 고유 ID, 양의 배점, 배점 합계, parameter 참조와 타입을 검사한다.
- 제공·제출 파일 경로는 세션 작업 디렉터리 안으로 제한한다. 상위 경로 이탈과 외부를 가리키는 symlink를 거부한다.
- setup·solution·cases의 리소스 범위를 선언한다. namespace 밖의 객체와 클러스터 범위 리소스는 명시적 허용 목록이 있어야 한다.
- 문제의 요구 capability가 환경 검증 결과와 맞아야 출제할 수 있다.
- 정답의 구문 자체를 요구한 문제가 아니라면 YAML 문자열·키 순서·임의 라벨 이름으로 정답을 제한하지 않는다.
- `replicas`, container image, resource quantity 등은 의미에 맞는 타입으로 비교한다. 예를 들어 `1 CPU`와 `1000m`을 문자열 차이로 오답 처리하지 않는다.

### 6.3 seed와 문제 version

세션 시작 시 문제 목록과 순서를 먼저 확정하고, 허용된 값·제약 안에서 parameter를 확정한다. 같은 확정 값이 문제·setup·검사·solution·오답 사례에 일관되게 반영된다.

저장 항목은 seed, 난수 처리 방식의 version, 문제 ID·version·내용 hash, 확정 parameter, 렌더링된 사용자 문제와 검사 정의, 환경 version manifest이다. 같은 seed만으로 미래의 변경된 문제은행까지 재현된다고 약속하지 않는다. 진행 중인 세션에는 문제은행 수정이 반영되지 않는다.

사용한 문제 package는 내용 hash를 키로 보관한다. 풀이 디렉터리에 내보내는 것은 영어 문제와 제공 파일뿐이다. 검사 정의·모범 풀이·오답 사례·해설은 별도 관리 영역에 두고 정상 CLI에서 모드별 공개 규칙을 적용한다. 명령 오류·진단 로그에도 이 내용이 미리 노출되지 않아야 한다. 로컬 소유자의 의도적인 파일 접근까지 막는 보안 격리는 범위 밖이다.

초기 랜덤화는 이름·namespace·포트와 검증된 제한적 변형에 집중한다. 문제 유형 자체를 runtime에 생성하지 않는다. 빌드 이미지, chart, fixture를 고정하고 외부 서비스의 가용성을 정답 조건으로 삼지 않는다.

## 7. 문제은행 검증과 출제

### 7.1 작성에서 출제까지

외부 도구 또는 사람이 문제를 작성 → 정적 schema 검사 → 임시 문제 환경 구성 → 동일한 grader로 검증 → 검증 기록 등록 → Runtime에서 사용한다. ChatGPT/Codex/Claude Code는 외부 작성 도구이며 애플리케이션 API 의존성이 아니다.

검증 결과는 문제 hash, schema·grader version, 환경 version, 검증한 seed/parameter 조합과 연결한다. 내용이나 의미가 달라지는 구성요소가 바뀌면 기존 검증을 자동으로 재사용하지 않는다. 진행 중인 훈련·시험 세션이 있으면 문제은행의 실제 클러스터 검증을 거부한다.

문제마다 필수 조건은 다음과 같다.

1. **Initial:** 초기화 직후 미해결이며, 정의된 초기 항목별 결과·점수와 일치한다. 부분 점수 구조상 일부 항목이 이미 맞을 수 있으나 문제 전체가 만점이면 무효다.
2. **Solved:** 모범 풀이 후 모든 필수 요구사항을 충족하고 만점이다.
3. **Wrong:** 중요한 오류를 심은 사례에서 지정한 검사만 또는 정의된 종속 검사들이 정확히 실패한다. 초기 상태와 정답만 통과하는 느슨한 grader를 허용하지 않는다.
4. **Reset:** 이전 실행의 리소스·파일이 다음 실행의 결과에 영향을 주지 않는다.
5. **Alternative:** 풀이가 여러 방식으로 가능하다면 대표적인 다른 정답도 통과한다.

대표 seed와 경계값을 검증하고 출제 전에 변수 제약을 검사한다. 제한된 seed 테스트를 모든 조합의 증명이라고 주장하지 않는다. 문제 시작 시에도 확정된 parameter에 대한 초기 상태 검증을 수행하며, 실패한 문제는 사용자에게 출제하지 않는다.

### 7.2 문제 구성과 배점

- 출시 문제은행에는 기본 20문항 모의시험을 구성할 수 있는 서로 다른 문제 정의가 있어야 하며, 공식 역량별로 적어도 하나의 검증된 문제에 연결되어야 한다. parameter만 바꾼 문제를 새로운 독립 문제 수로 세지 않는다.
- 기본 `mock-01`은 검토된 20문항 구성을 사용한다. 주 영역별 점수 합은 20/20/15/25/20, 총점은 100이다. YAML의 검사 배점과 시험에 표시되는 배점은 같아야 한다.
- 각 문제에는 주 영역 하나를 지정한다. 여러 역량을 다룰 수 있지만 domain 점수를 중복 합산하지 않는다. 다른 영역에서의 학습 효과는 tags·역량 매핑에 기록한다.
- Build, Modify, Troubleshoot, Behavioral 유형을 섞고 기본 시험에 네 유형을 모두 포함한다. 유형 비율을 실제 비공개 시험의 통계처럼 표현하지 않는다.
- 일반 훈련 세트는 5~20개를 선택할 수 있다. 오답 재도전은 1~20개를 허용한다. 선택된 문제의 원래 점수를 합산하고 실제 영역별 분포를 보여준다. 짧은 훈련을 공식 비율을 충족한 모의시험으로 부르지 않는다.
- 출제 가능한 문제 수가 부족하면 중복으로 몰래 채우지 않고 이유와 가능한 수를 표시한다.

Easy는 명확한 단일 개념 생성·수정, Medium은 리소스 관계나 2~3개 개념의 결합, Hard는 원인 진단과 여러 단계의 수정이다. 문제 길이나 불필요한 타이핑을 난이도로 삼지 않는다.

## 8. 채점 계약

### 8.1 판정과 점수

각 criterion은 ID, 대상, 요구 조건, 정수 배점, 선행 조건, 최대 대기 시간과 한국어 피드백을 가진다. criterion 자체는 전부 또는 0점이고, 문제의 부분 점수는 통과 criterion의 합이다. 더 세밀한 부분 점수가 필요하면 criterion을 나눈다.

| 판정 | 의미 | 점수 처리 |
| --- | --- | --- |
| PASS | 요구사항 충족 | 해당 배점 획득 |
| FAIL | 풀이 결과가 요구사항을 충족하지 않음 | 해당 항목 0점과 실제 관찰 근거 |
| ERROR | 관리되는 검사 환경·API·도구 장애 등으로 판정 불가 | 점수 확정 보류 |

리소스 부재로 하위 검사도 할 수 없다면 `FAIL: prerequisite not satisfied`로 기록한다. 사용자가 지정한 잘못된 이미지·selector·probe 때문에 앱이 실패한 것은 FAIL이다. 검사용 이미지 준비 실패나 클러스터 API 불통은 ERROR다. 원인을 판별할 근거가 부족하면 오답으로 추정하지 않고 ERROR로 처리한다.

ERROR가 하나라도 있으면 해당 제출은 미완료다. 확인된 항목 결과는 보관하되 확정 총점·합격 여부를 표시하지 않는다. ERROR 배점을 분모에서 빼지 않는다.

### 8.2 검사 종류와 동작

| 검사 | 핵심 기준 |
| --- | --- |
| Resource / Field | GVK·namespace·이름으로 객체 확인, 요구한 필드를 의미 기반 비교 |
| Workload readiness | 선언한 rollout 목표가 관찰됐는지와 ready/available 상태, 문제별 제한 시간 |
| HTTP / TCP | 지정 출발점에서 목적지에 실제 연결. HTTP는 응답 코드와 기대 애플리케이션 식별 결과 확인 |
| NetworkPolicy | 정상 서버·검사 Pod·대조 경로 확인 후 허용·차단 경로를 각각 검사 |
| Logs / File | 요구한 로그·산출물의 구조·내용 검사. 단순 전체 문자열 일치에 의존하지 않음 |
| Image | 요구 이미지·아카이브·설정 확인과 실제 실행. 소스 작성 방식은 명시된 경우만 제한 |
| Helm | release·chart·values·배포 상태 중 과제가 요구한 조건 확인 |
| Kustomize | 제출한 overlay를 렌더링해 의미를 검증하고 실제 배포 상태도 확인 |
| Auth / Security | 요구 권한·SecurityContext 등 설정과 필요 시 실제 사용자·동작 확인 |

검사용 Pod는 사용자 네트워크 문제의 selector·namespace 조건에 맞춰 만든다. 플랫폼의 숨은 정책으로 문제의 조건을 바꾸지 않는다. NetworkPolicy의 차단 성공은 timeout 하나만으로 확정하지 않는다. 동일 목적지의 정상 서비스 상태와 허용 경로를 함께 검증하며, 서버가 죽어 모두 차단된 상태는 정답이 아니다.

검사에 필요한 변경은 기록하고 정리한다. 사용자 답안을 자동 수정하거나 재시작해 정답으로 만들어주지 않는다. 파괴적인 동작 확인이 필요하면 해당 검사가 문제에 명시돼 있고 반복 가능한 fixture를 사용해야 한다. 무조건 Pod를 죽여 probe를 검사하는 일반 규칙은 두지 않는다.

Job·CronJob은 선언과 실행 결과를 구분한다. CronJob 템플릿에서 수동 Job을 생성해 성공한 것만으로 스케줄 발동을 증명하지 않는다. 스케줄 동작을 요구한 문제는 별도의 관찰 조건을 정의한다. 마찬가지로 최종 Deployment 상태만 보고 과거 rollback 명령 실행 자체를 증명했다고 주장하지 않는다.

### 8.3 시간·안정화·관찰 증거

결정적 채점이란 동일한 안정된 입력 상태에 명시된 검사 규칙을 적용하는 것이다. 비동기 클러스터를 제출 순간에 완벽하게 동결하거나 시간과 무관한 결과를 보장한다는 뜻은 아니다.

- 준비·재시도·timeout은 검사 정의에 고정한다. 무제한 대기와 평가 모델 호출은 없다.
- 일반 상태 수렴의 기본 대기 한도는 30초, 네트워크 요청의 개별 timeout은 3초·최대 3회로 시작한다. 개념상 더 긴 관찰이 필요한 문제는 명시적 한도를 선언하고 검증한다. 단순 지연을 보정하려고 timeout을 무제한 늘리지 않는다.
- 각 검사에 기대값, 실제값, 관찰 시각, 대상 UID, 관련 generation, 실행 시간과 실패 원인을 남긴다. Secret의 원문·토큰은 로그와 피드백에 출력하지 않는다.
- 채점 병렬성은 자원 예산에 맞춰 제한하고, 같은 리소스를 변경하는 동작 검사는 순차 실행한다.
- 제출 후에는 사용자가 수정을 멈추도록 안내한다. 주요 대상 spec·UID가 검사 중 바뀐 것이 감지되면 일관되지 않은 결과를 확정하지 않는다. 모든 수동 변경을 완벽하게 차단하는 시험 감독 기능은 제공하지 않는다.

## 9. 세션과 학습 흐름

### 9.1 상태와 타이머

세션은 `PREPARING → RUNNING → GRADING → COMPLETED`를 기본 흐름으로 갖는다. 준비 실패는 `SETUP_ERROR`, 채점 장애는 `GRADING_ERROR`, 사용자가 종료하고 새 시도를 선택하면 `ABANDONED`로 기록한다. 복구가 필요한 작업의 종류와 완료 단계를 함께 보관한다.

- `PREPARING`: 환경 capability, 이미지·제공 파일, 초기 객체·초기 채점 결과를 확인한다. 의도적으로 고장 낸 앱은 문제의 예상 초기 상태와 비교한다.
- `RUNNING`: 문제 준비가 끝나고 사용자가 시작을 확인한 시점에 시작 시간을 기록한다. 사용자는 문제를 자유롭게 이동한다.
- 준비 완료 화면에는 문제 수·시간·환경 정보만 보여주고, 실제 문제 본문과 제공 파일은 시작 확인 후 풀이 경로에 공개한다. 준비 시간에 문제를 먼저 읽는 것이 기본 흐름이 되지 않게 한다.
- 터미널 종료·호스트 sleep에도 모의시험 시간은 계속 흐른다. 벽시계 기준 시작·제출 시각을 UTC로 기록하고 표시할 때 로컬 시간대로 변환한다. 시계 역행 등 명백한 이상은 결과에 표시한다.
- `GRADING`: 제출 시 풀이 종료 시각을 기록한다. 이 제출의 풀이 시간은 시작부터 제출까지이며 채점 실행 시간은 별도로 기록한다.
- 제한시간 초과는 제출을 막지 않는다. `duration`, `overtime`, 제한시간 내 제출 여부를 함께 표시한다.

채점 오류 후에는 동일 세션에서 재시도할 수 있다. 각 재시도는 별도 grading run으로 기록하고 최초 제출 시각과 새로운 관찰 시각을 모두 보여준다. 재시도는 현재의 실제 클러스터를 다시 검사하므로 최초 제출 순간의 상태를 복구했다고 주장하지 않는다. 답안을 수정하고 재제출하려면 새 제출로 기록하고 새 제출 시각을 사용한다. 이전 제출·실패 기록은 덮어쓰지 않는다.

`--retry`는 수정하지 않은 답안의 기술적 채점 재시도이고 `--resubmit`은 수정 후 새 제출이다. 주요 답안 입력이 달라진 것이 확인되면 retry를 거부하고 resubmit을 안내한다. 파일·이미지 등 API 밖의 산출물도 해당 검사가 기록한 식별 정보로 비교한다. 이 구분은 학습 기록의 의미를 보존하기 위한 것이며 악의적인 변경 탐지 보장은 아니다.

### 9.2 모드별 공개·성적

| 기능 | 모의시험 | 훈련 |
| --- | --- | --- |
| 문제별 채점 | 불가 | 가능 |
| 제출 전 세부 실패 원인 | 비공개 | 채점 요청 시 공개 |
| 해설·모범 답안 | 전체 제출 완료 후 열람 | 사용자가 명시적으로 열람 가능 |
| 문제 표시·북마크 | 가능, 자기 표시일 뿐 자동 정답 판정 아님 | 가능 |
| 반복 채점 | 제출 후 새 제출·복습 기록으로 구분 | 각 실행을 기록 |
| 초기화 | 진행 중 시험의 개별 문제 reset은 불가. 시험 포기·새 시도 필요 | 문제별 reset 가능 |

모의시험 중 `doctor`는 플랫폼 상태만 안내하고 문제의 숨은 판정·정답을 노출하지 않는다. ERROR가 남아 있는 모의시험에는 세부 정답 해설을 공개하지 않는다. 시험을 포기하고 복습으로 전환하는 경우 그 상태를 기록한 뒤 공개한다.

훈련에서는 개별 문제의 최신 결과와 관찰 시각을 보여준다. 서로 다른 시점의 최고 점수를 합쳐 하나의 시험 점수로 만들지 않는다. 훈련 세트 전체의 일관된 총점이 필요하면 전체 채점을 새로 실행한다. 해설 열람 이력은 재도전에서 과거 학습 이력으로 남으며 처음 보는 문제처럼 표시하지 않는다.

### 9.3 오답 복습과 재도전

복습은 실패한 요구사항 → 실제 관찰 결과 → 진단에 사용할 명령·문서 → 원인 설명 → 모범 풀이 순으로 제공한다. 단순 정답 YAML만 보여주지 않는다. 해설은 사용자가 명시적으로 열도록 해, 실패 근거만 보고 다시 풀 수도 있게 한다.

같은 조건 재도전은 문제 version과 확정 parameter를 재사용한다. 변형 재도전은 검증된 범위에서 새 seed를 사용한다. 부분 점수 문제도 오답 선택 대상이다. ERROR만 발생한 문제를 약점으로 분류하지 않는다. 과거 문제 아티팩트가 없어 정확한 재현이 불가능하면 다른 version으로 몰래 대체하지 않는다.

## 10. CLI 계약

아래는 사용자 동작의 계약이며 구현 순서나 코드 scaffold가 아니다. `ckad start`의 기본값은 `mock-01`이다.

| 명령 | 역할 |
| --- | --- |
| `ckad doctor` | 도구·환경 version, 전용 클러스터 식별, 플랫폼 capability·자원 상태 확인 |
| `ckad list` | 출제 가능한 문제·모의시험 조회. 정답 비노출 |
| `ckad start [mock-id]` | 기본 모의시험 준비·시작 |
| `ckad start --mode practice --count 5` | 짧은 훈련. domain·difficulty·tag·seed 필터 지원 |
| `ckad start --retry <session-id> --failed` | 오답을 새 훈련으로 시작. 기본은 같은 조건, `--new-seed`로 변형 |
| `ckad status` | 모드, 문제 목록, 배점, 표시 상태, 경과·초과 시간 |
| `ckad show Q4` | 문제·대상 context·namespace·제공 파일 조회 |
| `ckad mark Q4 --review` | 나중에 볼 문제 표시. `--clear`로 해제 |
| `ckad grade` | 전체 최초 제출·채점. 이미 제출된 세션에서는 기존 결과와 다음 명령을 안내 |
| `ckad grade --retry` / `--resubmit` | 오류 제출의 기술적 재시도 / 수정 후 새 제출을 명시적으로 구분 |
| `ckad grade --problem Q4` | 훈련의 개별 문제 채점 |
| `ckad review [session-id]` | 제출·훈련 결과와 실패 근거 조회 |
| `ckad review --problem Q4 --solution` | 모드·상태에 따라 해설과 모범 풀이 공개, 열람 기록 |
| `ckad reset --problem Q4` | 훈련 문제를 초기화하고 새 시도로 기록 |
| `ckad reset` | 현재 세션을 끝내고 소유 리소스를 정리. 재시작은 start에서 수행 |
| `ckad reset --cluster` | 전용 클러스터의 복구용 재생성. 대상과 영향 확인 필요 |
| `ckad bank validate <problem-id>` | 작성용 검증. 진행 중 학습과 동시 실행 불가 |

준비·채점·reset은 progress와 원인을 출력하고 중단될 수 있다. 잘못된 모드의 명령, 부족한 문제 수, 연결 실패, 자원 부족은 원인과 다음 행동을 한국어로 안내한다. 전체 stack trace를 기본 사용자 화면에 출력하지 않는다.

## 11. 저장·중단 복구

SQLite는 작업 컨테이너의 전용 named volume에 보관한다. 문제 원본은 Git 관리 대상이고 세션 DB·사용자 답안은 Git에 커밋하지 않는다.

저장 개념은 environment, session, session problem, submission/grading run, criterion result, review event, owned resource inventory, problem validation이다. 세션별 확정 문제와 parameter를 snapshot으로 보관하고, 제출별 결과·증거를 불변 기록으로 남긴다. 재도전은 부모 session/problem 관계로 연결한다.

- 환경 하나에 변경 작업 잠금 하나를 두어 start·reset·bank validation·grade가 충돌하지 않게 한다. 여러 터미널의 조회는 허용한다.
- 외부 Kubernetes 변경과 SQLite 트랜잭션을 하나의 원자적 작업으로 간주하지 않는다. 작업 의도와 단계를 먼저 기록하고 실제 상태를 확인하며 재개한다.
- 세션 상태·최종 점수의 갱신은 트랜잭션으로 처리한다. 일부 검사만 저장된 상태를 완료된 성적으로 표시하지 않는다.
- 프로세스 중단 후 다음 명령은 미완료 작업을 감지하고 복구 방법을 안내한다. 누락된 namespace·클러스터를 답안과 함께 자동으로 새로 만들지 않는다.
- 깨진 DB를 빈 DB로 덮어쓰지 않는다. 오류를 알리고 원본을 보존한다.
- `.bashrc`와 개인 파일은 런타임 갱신 시 덮어쓰지 않는다. 기본 설정은 최초 실행에만 제공한다.

## 12. Reset·격리·소유권

### 12.1 문제 격리

기본 단위는 문제별 namespace다. cross-namespace 네트워크 문제는 문제 소유 namespace 묶음을 하나로 관리한다. 문제 간 리소스 의존성은 만들지 않는다. 공유 시스템 구성요소는 platform 소유이며 문제 reset 대상이 아니다.

namespace 삭제 완료를 확인한 뒤 새로 생성한다. 같은 이름만 만들었다고 reset 완료로 보지 않는다. 소유 클러스터 범위 리소스, PVC에 연결된 저장 데이터, 빌드 이미지·제공 파일도 문제 정의에 맞게 초기화한다. PVC의 reclaim 정책에 따라 PV·데이터가 남는 경우까지 검사한다.

풀이 파일은 삭제 대신 기존 attempt 디렉터리에 보관하고 새 attempt 디렉터리를 만든다. 개인 설정과 성적은 그대로 남는다. namespace가 정리되지 않으면 오류와 남은 대상을 보여주며 finalizer를 무조건 제거하지 않는다.

### 12.2 다른 환경 보호

- 호스트의 기존 kubeconfig를 가져오거나 current-context를 변경하지 않는다. JunKloud 전용 경로와 명시적 context를 사용한다.
- 환경 생성 시 임의 environment ID, 실제 API endpoint·CA 식별, 클러스터 안의 식별용 객체 UID와 Docker 관리 대상 ID를 기록한다.
- 변경·삭제 전에 이 정보를 비교한다. 이름이나 `junkloud-*` 접두사 하나만으로 소유권을 판단하지 않는다.
- 문제 리소스에는 session/problem 소유 표시와 생성 결과를 기록한다. namespace 소유권을 확인한 후 그 안의 사용자 생성 리소스를 정리한다. 클러스터 범위 객체는 명시적으로 기록한 대상만 처리한다.
- 대상 식별 정보가 다르면 중단한다. 자동으로 현재 context를 대체 대상으로 선택하지 않는다.
- Docker의 전역 `system prune`, 전체 container/image/volume 삭제는 사용하지 않는다. 전용 리소스의 확인된 ID·소유 표시로 정리한다. 공유 이미지 layer나 전체 build cache를 제거하지 않는다.

작업 컨테이너의 환경 관리와 이미지 실습은 Docker daemon 접근이 필요하다. **Docker 소켓 공유는 같은 daemon의 다른 리소스에 대한 강한 권한을 제공하므로 보안 격리 경계가 아니다.** 위 규칙은 JunKloud의 자동 작업에서 사고를 예방하는 장치다. 사용자가 직접 실행한 임의 Docker 명령이나 신뢰하지 않는 문제 작성 코드를 악의적으로 격리하는 기능은 MVP에 포함하지 않는다. 외부에서 작성한 문제는 로컬 신뢰 검토와 schema·작업 종류 검증 후 사용한다.

## 13. 검증 전략과 출시 기준

이 문서 작성 시점에는 제품 코드, 클러스터 실측, 런타임 테스트가 없다. 아래는 구현 후 통과해야 할 기준이며 현재 통과 사실을 의미하지 않는다.

| 검증 층위 | 요구 사항 |
| --- | --- |
| 순수 로직 | schema·parameter 제약·배점·영역 집계·시간 계산·공개 규칙의 경계값 검사 |
| Grader 단위 | PASS/FAIL/ERROR, 종속 조건, 수량·배열 비교, retry·timeout, Secret 비노출 |
| API 연동 | 실제 API의 기본값·권한·리소스 version·삭제 동작 검증. fake client만으로 완료 판단하지 않음 |
| 문제 검증 | 모든 출제 문제의 Initial/Solved/Wrong/Reset, 필요한 대체 정답과 대표 parameter 검증 |
| 환경 기능 | DNS·Service·Ingress·NetworkPolicy 허용/차단·볼륨·metrics·이미지 실습 검증 |
| 세션 통합 | start→풀이→grade→review→reset→retry, 모드별 공개와 시간 초과 처리 |
| 장애 복구 | 준비·채점·정리 중 중단, API 불통, 검사 도구 실패, 저장 실패, 중복 명령 |
| 안전 | 다른 클러스터 식별·잘못된 context·동명 리소스 거부, 소유하지 않은 Docker 대상 보존 |
| 호환성·부하 | M2 8GB ARM64와 Windows 16GB의 실제 환경에서 20문항 동시 준비·채점·반복 초기화 |

기본 모의시험은 네 문제 유형을 모두 포함하고 공식 영역 배점을 충족해야 한다. 문제은행 전체는 3장의 역량 매핑을 충족해야 한다. 단일 노드에서 검증할 수 없는 고급 다중 노드 현상은 지원하는 것처럼 출제하지 않는다.

기본 세트에 대해 준비→모범 풀이→채점→reset을 최소 3회 연속 수행해 결과 일치, 잔여 문제 리소스·데이터 없음, 명백한 자원 누적 없음, OOM·환경 원인의 Pending 없음과 개인 파일 보존을 확인한다. 장애를 심는 테스트는 별도로 수행한다.

시간 측정은 최초 다운로드가 포함된 cold start와 이미지가 준비된 warm start를 구분한다. warm start, 문제별·전체 reset, 전체 grade의 시간과 peak 메모리를 기록한다. 현재 성능 측정치가 없으므로 특정 초 단위 속도를 보장하지 않는다. M2 8GB·4GiB VM 예산을 통과하지 못하면 완료로 표시하지 않고 문제 fixture·구성요소를 조정하거나 대안을 설계 검토한다.

## 14. Post-MVP

실제 다중 클러스터·context 전환, SSH 진입·복귀 훈련, 약점 기반 sampling, spaced repetition, mastery tracking, 외부 AI generation adapter는 후속 범위다. 이를 위한 일반 plugin framework·원격 서버·추가 DB는 미리 만들지 않는다.

MVP의 문제 schema·version·결과 기록만으로 향후 확장의 근거를 남긴다. 기술적 확장성보다 실제 학습 기록에서 확인된 필요를 다음 기능의 기준으로 삼는다.

## 15. 설계 검토와 다음 단계

이 문서는 합의한 방향에 구체적인 CLI 계약·오류 처리·검증 기준을 붙인 승인된 설계다. 2026-10-14에 사용자가 구현 계획 작성을 요청했다. 구현 계획 검토와 실행 방식 선택 전 제품 코드·의존성 설치를 시작하지 않는다.

공식 curriculum PDF 본문과 역량 매핑의 교차 확인은 완료했다. implementation plan에는 버전 고정 및 환경 capability 검증을 포함한다. 구체적인 SQLite driver·CLI parser 등 교체 가능한 내부 라이브러리는 그 계획에서 선택한다. 환경 기능과 문제 채점 계약을 바꾸는 선택은 이 설계의 변경으로 기록한다.

## 16. 공식 참고 자료

확인일: 2026-10-14. 버전·규칙은 변경될 수 있으므로 환경 version manifest를 갱신할 때 다시 확인한다.

- [S1 — Linux Foundation CKAD FAQ](https://docs.linuxfoundation.org/tc-docs/certification/faq-cka-ckad-cks): 시험 버전·시간·합격 기준.
- [S2 — Important Instructions: CKA and CKAD](https://docs.linuxfoundation.org/tc-docs/certification/tips-cka-and-ckad): 과제 수·Linux CLI·SSH 작업 흐름·기본 도구·시험 버전.
- [S3 — CNCF CKAD Curriculum](https://github.com/cncf/curriculum/tree/master/ckad), [v1.37 PDF 게시 위치](https://github.com/cncf/curriculum/blob/master/CKAD_Curriculum_v1.37.pdf): 영역별 배점과 공식 curriculum. 본문·페이지 확인 결과와 hash는 3.1에 명시.
- [S4 — Linux Foundation CKAD](https://training.linuxfoundation.org/certification/certified-kubernetes-application-developer-ckad/): Domains & Competencies. 상품 페이지의 버전 표기 차이는 3.1 참고.
- [S5 — Resources Allowed](https://docs.linuxfoundation.org/tc-docs/certification/certification-resources-allowed): CKAD 허용 문서와 검색 범위.
- [S6 — Candidate Handbook: Exam Rules and Policies](https://docs.linuxfoundation.org/tc-docs/certification/lf-handbook2/exam-rules-and-policies): 공개 시험 규칙.
- [S7 — kind v0.24 release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.24.0): 기본 NetworkPolicy 지원 추가.
- [S8 — kind v0.33 release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0): 초기 검증 후보 Kubernetes 버전과 아키텍처·digest 고정 안내.
- [S9 — kind Ingress](https://kind.sigs.k8s.io/docs/user/ingress/): cloud-provider-kind의 Ingress 지원 경로.
- [S10 — K3s Networking Services](https://docs.k3s.io/networking/networking-services), [K3s Requirements](https://docs.k3s.io/installation/requirements): 대안의 네트워크 기능·기본 자원 요구.
- [S11 — minikube NetworkPolicy](https://minikube.sigs.k8s.io/docs/handbook/network_policy/): 대안의 CNI 요구사항.
- [S12 — Kubernetes Client Libraries](https://kubernetes.io/docs/reference/using-api/client-libraries/): 공식 Go·Python 클라이언트.
- [S13 — client-go](https://github.com/kubernetes/client-go): API 클라이언트와 version 호환 정책.
- [S14 — Appropriate Uses for SQLite](https://www.sqlite.org/whentouse.html): 서버 없는 로컬 애플리케이션 저장 용도.
- [S15 — Docker Desktop for Mac](https://docs.docker.com/desktop/setup/install/mac-install/), [Windows](https://docs.docker.com/desktop/setup/install/windows-install/): 호스트·아키텍처·Linux backend 요구사항.
