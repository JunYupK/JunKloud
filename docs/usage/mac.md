# macOS 사용 안내

이 문서는 macOS(Docker Desktop)에서 JunKloud를 시작하는 방법의 기기별 세부 사항입니다. 전체 흐름은 [README](../../README.md)를, 훈련 명령은 [훈련 흐름 안내](training.md)를 보세요.

> **검증 상태.** Apple M2(ARM64) 8GB에 4GiB Docker VM에서의 실행은 **아직 하지 않았습니다**(미완료). 아래 4GiB는 목표 예산이며 동작 보증이 아닙니다. 결과 칸은 [수용 시험 매트릭스](../validation/acceptance-matrix.md)에 있습니다.

## 1. Docker Desktop 설정

1. Docker Desktop을 설치하고 실행합니다. macOS에서는 Linux 컨테이너만 지원하므로 모드를 바꿀 필요가 없습니다.
2. **VM 메모리를 4GiB 이상**으로 둡니다(Settings → Resources → Memory. 메뉴 이름은 버전에 따라 다를 수 있습니다). 4GiB는 설계 문서 §4.3의 초기 검증 예산입니다. 여유가 있으면 더 크게 잡으세요. 현재 값은 `docker info --format '{{.MemTotal}}'`(바이트)로 확인합니다.
3. 디스크 여유를 넉넉히 둡니다. 필요한 정확한 용량은 아직 측정하지 않았습니다.
4. 확인:

   ```sh
   docker version
   docker compose version
   ```

Apple Silicon에서는 Linux ARM64 이미지를 **네이티브로** 실행합니다. 문제에 쓰는 이미지와 도구는 CPU 아키텍처에 맞게 받으며, 에뮬레이션(QEMU 등)으로 숨기지 않습니다. 작업 컨테이너의 도구 바이너리도 아키텍처별로 SHA-256을 고정해 받습니다([`runtime/versions.lock.yaml`](../../runtime/versions.lock.yaml)).

## 2. 시작

```sh
git clone https://github.com/JunYupK/JunKloud.git
cd JunKloud
bash runtime/up.sh
docker compose exec workspace bash
```

`bash runtime/up.sh`는 다음을 합니다.

- `kind` Docker 네트워크(bridge)가 없을 때만 새로 만듭니다(기존 네트워크는 변경·삭제하지 않음. bridge가 아니면 중단).
- `docker compose up -d --build workspace`로 작업 컨테이너를 빌드하고 띄웁니다. 저장소가 컨테이너의 `/src`에 연결됩니다.
- `learner-home` Docker 볼륨에 홈 디렉터리를 보관합니다.

개발·검증용 컨테이너는 `bash runtime/up.sh dev`, 이어서 `docker compose exec dev bash`입니다.

## 3. 두 번째 터미널

새 터미널 창에서 같은 컨테이너에 다시 들어갑니다(저장소 폴더에서).

```sh
docker compose exec workspace bash
```

`ckad status`, `show`, `review`는 여러 터미널에서 함께 조회해도 됩니다. `start`, `grade`, `reset`, `bank validate`는 한 번에 하나만 실행됩니다. 일반 터미널 접속이며 SSH 훈련이 아닙니다.

## 4. `k` 별칭과 자동완성

작업 컨테이너의 모든 대화형 bash에는 실제 시험처럼 다음이 미리 설정돼 있습니다([`runtime/kubectl-shell.bash`](../../runtime/kubectl-shell.bash)).

- `alias k=kubectl`
- `kubectl`과 `k`의 Tab 자동완성

```sh
k get pods -A
k get po<Tab>
```

리소스 이름 자동완성(`k get pods <Tab>`)은 클러스터에 물어보므로 `ckad doctor`나 `ckad start`로 클러스터가 준비된 뒤에 동작합니다. 개인 별칭과 편집기 설정은 `~/.bashrc`, `~/.bash_aliases`에 추가하면 컨테이너를 다시 만들어도 유지됩니다(기본 `k`도 여기서 덮어쓸 수 있습니다).

## 5. Docker 소켓의 권한 범위

작업 컨테이너는 호스트의 `/var/run/docker.sock`을 공유합니다. 시작 스크립트의 일회성 단계가 소켓 권한을 컨테이너의 일반 사용자(`learner`)가 쓸 수 있게 완화합니다.

- 소켓에 접근할 수 있다는 것은 같은 Docker 엔진의 다른 컨테이너·이미지·볼륨에 강한 권한을 갖는다는 뜻입니다. **이것은 보안 격리 경계가 아닙니다.**
- JunKloud 자체는 자신이 만들고 소유를 기록한 리소스(환경 ID·라벨·확인된 ID)만 정리하며, 전역 `docker system prune`이나 전체 삭제는 쓰지 않습니다.
- 그러나 작업 컨테이너 안에서 직접 입력한 Docker 명령에는 이 보호가 적용되지 않습니다. 중요한 컨테이너·볼륨이 있는 Docker라면 주의하세요.

## 6. 범위 밖

- 실제 시험의 SSH 진입·복귀는 재현하지 않습니다. 모든 문제는 작업 컨테이너의 셸 하나에서 풉니다.
- 클러스터 context 전환 훈련은 제공하지 않습니다. 전용 클러스터 하나만 쓰며, 호스트의 kubeconfig나 current-context를 가져오거나 바꾸지 않습니다. 문제의 `Context:` 줄은 이 전용 클러스터의 context입니다.

## 7. 컴퓨터를 재시작한 뒤, 지우기

[README 2.5](../../README.md#25-컴퓨터를-재시작한-뒤)와 [2.6](../../README.md#26-컨테이너-멈추기와-완전히-지우기)을 보세요. 요약:

- 재시작 뒤 `bash runtime/up.sh`를 다시 실행하고 컨테이너에 들어가 `ckad status`로 확인합니다.
- `docker compose down`은 컨테이너만 지웁니다. `docker compose down -v`는 `learner-home` 볼륨(풀이·기록)을 지우며 되돌릴 수 없습니다. kind 클러스터는 어느 쪽도 지우지 않습니다.
