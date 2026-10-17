# Linux 작업 환경 시작

호스트에는 Docker Desktop의 Linux containers와 Docker Compose가 필요합니다.
준비 단계만 호스트에서 실행하며 `ckad`, 셸, `kubectl`, 채점은 모두 같은 Linux
컨테이너 안에서 실행합니다.

Windows PowerShell에서 저장소 루트 기준:

```powershell
powershell -ExecutionPolicy Bypass -File runtime/up.ps1
docker compose exec workspace bash
```

macOS 또는 Linux 터미널에서:

```sh
bash runtime/up.sh
docker compose exec workspace bash
```

시작 스크립트는 `kind` bridge가 없을 때만 생성하고 workspace를 연결합니다.
기존 네트워크 설정은 바꾸지 않습니다. `docker compose down`도 이 공유 네트워크를
삭제하지 않습니다. 셸·개인 풀이·기록은 learner-home 볼륨에 보존됩니다.
두 번째 호스트 터미널에서도 `docker compose exec workspace bash`로 들어가
동일한 세션의 `ckad status`, `show`, `review`를 조회할 수 있습니다.

컨테이너 안에서:

```sh
ckad --help
ckad doctor
ckad list
ckad start                     # 기본 mock-01, 시작 전 확인
ckad show Q4
kubectl get namespaces         # JunKloud 전용 kubeconfig
ckad grade
ckad review
ckad review --problem Q4 --solution
```

이 커밋에는 제품 문제 은행이 아직 없습니다. 작성된 은행은 `/src/problems/`와
`/src/exams/`에 두며, 개발 fixture만 확인하려면 컨테이너에서
`export CKAD_BANK_DIR=/src/tests/fixtures/dev-bank` 후 각 문제 ID에 대해
`ckad bank validate <problem-id>`를 실행합니다. fixture는 5개 문제의
`dev-mock` 모의시험과 훈련용이며 제품 모의시험으로 표시하지 않습니다.

기본 상태 경로는 `$HOME/.local/share/junkloud`, 풀이 경로는 `$HOME/ckad`입니다.
각각 `CKAD_STATE_DIR`, `CKAD_WORKSPACE`, 문제 은행은 `CKAD_BANK_DIR`로
변경할 수 있습니다. `--state-dir`, `--workspace`, `--bank-dir` 옵션도 지원합니다.
workspace 진입 시 `kubectl`도 전용 kubeconfig를 사용합니다. 상태 경로를 셸 안에서
변경했다면 `export KUBECONFIG="$CKAD_STATE_DIR/environment/kubeconfig"`도 적용하세요.
풀이 파일은 세션/문제별 `attempt-1` 경로에 보존됩니다. `show`는 영어 문제와
제공 파일을 표시하며 해설은 명시적인 `review --solution`에서만 공개합니다.

종료 코드는 0(조작 성공 또는 만점), 1(정상 채점의 미충족), 2(사용법·모드 오류),
3(환경·저장·채점 불가)입니다. ERROR가 있으면 확정 총점을 표시하지 않습니다.
`grade --retry`는 수정하지 않은 오류 제출을 다시 관찰하고, `--resubmit`은
수정 후 새 제출을 기록합니다. 모의시험의 66%는 참고 수치입니다.

`reset`, 오답 재도전(`start --retry`)은 후속 Task 13의 연결 전까지 오류를
반환하며 정리 완료로 표시하지 않습니다. 직접 클러스터나 볼륨을 지우지 말고
현재 기록과 개인 파일을 보존하세요.

개발 환경은 `runtime/up.ps1 -Service dev` 또는 `bash runtime/up.sh dev`로 시작합니다.
그 안에서 `go test ./...`, `make test-runtime`, `make test-e2e`를 실행할 수 있습니다.
E2E는 실제 Docker·kind·kubectl과 클러스터 자원이 필요하며 사용할 수 없으면
실패를 반환합니다. 생략된 검증을 통과로 표시하지 않습니다.
`make test-e2e`는 고정 이미지 준비와 문제 검증을 포함하므로 Go 테스트의 기본
10분 대신 명시적인 25분 상한을 사용합니다.
