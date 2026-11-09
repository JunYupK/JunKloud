# Windows 사용 안내

Windows(Docker Desktop)에서 JunKloud를 시작하는 방법의 기기별 세부 사항입니다. 전체 흐름은 [README](../../README.md)를, 훈련 명령은 [훈련 흐름 안내](training.md)를 보세요.

> **검증 상태.** Windows 16GB 기기에서의 실행은 **아직 하지 않았습니다**(미완료). 시작 스크립트의 Windows PowerShell 5.1 동작을 시험하는 스크립트(`tests/runtime/workspace_windows_test.ps1`)는 있지만 이 문서를 쓰는 시점에 Windows 기기에서 실행한 기록이 없습니다. 결과 칸은 [수용 시험 매트릭스](../validation/acceptance-matrix.md)에 있습니다.

## 1. Docker Desktop 설정

1. Docker Desktop을 설치하고 실행합니다.
2. **Linux containers** 모드인지 확인합니다. 트레이 아이콘 메뉴에 `Switch to Windows containers...`가 보이면 현재 Linux 모드입니다. 반대로 `Switch to Linux containers...`가 보이면 Windows 모드이므로 전환하세요.
3. Docker Linux VM의 메모리를 **4GiB 이상**으로 둡니다. 4GiB는 설계 문서 §4.3의 초기 검증 예산이며 실측 소비량이나 동작 보증이 아닙니다.
   - WSL 2 백엔드를 쓰면 메모리 한도는 Docker Desktop 화면이 아니라 사용자 폴더의 `.wslconfig`로 정할 수 있습니다. 설정 방법은 Docker·Microsoft 공식 문서를 확인하세요(버전에 따라 다릅니다).
   - 현재 값은 `docker info --format '{{.MemTotal}}'`(바이트)로 확인합니다.
4. 확인(PowerShell):

   ```powershell
   docker version
   docker compose version
   ```

Windows는 확인된 CPU에 맞는 Linux 아키텍처를 네이티브로 씁니다. 에뮬레이션으로 숨기지 않습니다.

## 2. 시작 (PowerShell, 저장소 루트)

```powershell
git clone https://github.com/JunYupK/JunKloud.git
cd JunKloud
powershell -ExecutionPolicy Bypass -File runtime/up.ps1
docker compose exec workspace bash
```

- 저장소는 줄바꿈을 LF로 고정([`.gitattributes`](../../.gitattributes))하므로 `core.autocrlf=true`인 Windows에서 받아도 컨테이너 안의 셸 스크립트가 깨지지 않습니다.
- `up.ps1`은 `kind` bridge 네트워크가 없을 때만 만들고(이미 있는 네트워크는 변경하지 않음, bridge가 아니면 중단), `docker compose up -d --build workspace`를 실행합니다.
- 개발·검증용 컨테이너는 `powershell -ExecutionPolicy Bypass -File runtime/up.ps1 -Service dev`, 이어서 `docker compose exec dev bash`입니다.
- 이후의 `ckad`, `kubectl`, `k`는 모두 **컨테이너 안(bash)** 에서 입력합니다. PowerShell에서 `ckad`를 치면 동작하지 않습니다.

## 3. 두 번째 터미널

새 PowerShell 창을 열어 저장소 폴더에서 같은 컨테이너에 다시 들어갑니다.

```powershell
docker compose exec workspace bash
```

조회(`ckad status`, `show`, `review`)는 여러 터미널에서 해도 됩니다. `start`, `grade`, `reset`, `bank validate`는 한 번에 하나만 실행됩니다. 일반 터미널 접속이며 SSH 훈련이 아닙니다.

## 4. `k` 별칭과 자동완성

작업 컨테이너의 모든 대화형 bash에 `alias k=kubectl`과 `kubectl`·`k`의 Tab 자동완성이 미리 설정돼 있습니다([`runtime/kubectl-shell.bash`](../../runtime/kubectl-shell.bash)). 리소스 이름 자동완성은 클러스터에 물어보므로 `ckad doctor`나 `ckad start`로 클러스터가 준비된 뒤에 동작합니다. 개인 별칭과 편집기 설정은 컨테이너 안의 `~/.bashrc`, `~/.bash_aliases`에 추가하면 유지됩니다.

## 5. Docker 소켓의 권한 범위

작업 컨테이너는 Docker 엔진 소켓(`/var/run/docker.sock`)을 공유합니다. Docker Desktop의 Windows 브리지에서는 소켓이 root 전용 권한으로 오므로 시작 스크립트의 일회성 단계가 컨테이너의 일반 사용자(`learner`)가 쓸 수 있게 권한을 완화합니다.

- 소켓에 접근할 수 있다는 것은 같은 Docker 엔진의 다른 컨테이너·이미지·볼륨에 강한 권한을 갖는다는 뜻입니다. **보안 격리 경계가 아닙니다.**
- JunKloud 자체는 자신이 만들고 소유를 기록한 리소스만 정리하고 전역 `prune`은 쓰지 않습니다. 그러나 컨테이너 안에서 직접 입력한 Docker 명령에는 이 보호가 적용되지 않습니다.

## 6. 범위 밖

- 실제 시험의 SSH 진입·복귀는 재현하지 않습니다. 모든 문제는 작업 컨테이너의 셸 하나에서 풉니다.
- 클러스터 context 전환 훈련은 제공하지 않습니다. 전용 클러스터 하나만 쓰며 호스트의 kubeconfig나 current-context를 가져오거나 바꾸지 않습니다.

## 7. 컴퓨터를 재시작한 뒤, 지우기

[README 2.5](../../README.md#25-컴퓨터를-재시작한-뒤)와 [2.6](../../README.md#26-컨테이너-멈추기와-완전히-지우기)을 보세요. 요약:

- 재시작 뒤 Docker Desktop을 실행하고 `runtime/up.ps1`을 다시 실행한 다음 컨테이너에 들어가 `ckad status`로 확인합니다.
- `docker compose down`은 컨테이너만 지웁니다. `docker compose down -v`는 `learner-home` 볼륨(풀이·기록)을 지우며 되돌릴 수 없습니다. kind 클러스터는 어느 쪽도 지우지 않습니다.
