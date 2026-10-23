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
ckad start --mode practice --count 5  # 검증된 혼합 은행에서 5문제, 시작 전 확인
ckad show Q4
kubectl get namespaces         # JunKloud 전용 kubeconfig
ckad grade
ckad review
ckad review --problem Q4 --solution
```

제품 은행 루트는 `/src`이며 그 안의 `problems/`에서 패키지를 읽습니다.
현재 Design & Build 5문제/20점, Deployment 4문제/20점(rollout, release,
Helm, Kustomize), Observability 3문제/15점(API migration, probes, diagnostics),
합계 12문제/55점의 부분 은행이 포함되어 있습니다. 먼저 동일
환경에서 각 패키지를 검증하세요. 이전 grader의 검증 기록은 새 grader에서
유효하지 않으므로 현재 환경에서 아래 ID별 명령으로 다시 검증해야 합니다.

```sh
export CKAD_BANK_DIR=/src
for id in image-001 batch-001 daemonset-001 multicontainer-001 volume-001 \
          rollout-001 release-001 helm-001 kustomize-001 \
          api-migration-001 probes-001 diagnostics-001; do
  ckad bank validate "$id" || break
done
ckad list
ckad start --mode practice --count 5
```

Deployment 4문제나 Observability 3문제만으로는 일반 훈련의 최소 5문제를 충족하지 않습니다.
검증된 혼합 은행에서 5개 이상 선택하거나 더 큰 은행을 사용하세요.
이 구성은 20문제 제품 모의시험을 아직 제공하지 않습니다. 개발 fixture를
확인하려면 `CKAD_BANK_DIR=/src/tests/fixtures/dev-bank`를 사용하세요.
fixture의 `dev-mock`은 제품 모의시험이 아닙니다.

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

훈련 문제는 `ckad reset --problem Q1`로 초기화합니다. 기존 풀이·성적·개인
설정은 보존하고 새 attempt를 만들며, 진행 중 모의시험의 개별 reset은 거부합니다.
`ckad reset`은 환경 ID를 입력해 세션 포기와 전체 문제 정리를 확인합니다.
`ckad reset --cluster`도 환경 ID 확인 후 전용 클러스터를 재생성합니다.
중단된 초기화는 `ckad reset --recover`로 재개하며, 정리를 확인할 수 없는
PVC/PV나 finalizer는 오류와 기록을 남깁니다. 미완료 정리 중 새 학습은 시작하지 않습니다.
오답은 `ckad start --retry <session-id> --failed`로 같은 문제 version·조건에서
재도전하고, `--new-seed`를 붙이면 검증된 범위의 새 조건을 사용합니다.
원래 문제 패키지가 없으면 재도전을 거부합니다.

개발 환경은 `runtime/up.ps1 -Service dev` 또는 `bash runtime/up.sh dev`로 시작합니다.
그 안에서 `go test ./...`, `make test-runtime`, `make test-e2e`를 실행할 수 있습니다.
E2E는 실제 Docker·kind·kubectl과 클러스터 자원이 필요하며 사용할 수 없으면
실패를 반환합니다. 생략된 검증을 통과로 표시하지 않습니다.
`make test-e2e`는 고정 이미지 준비와 문제 검증을 포함하므로 Go 테스트의 기본
10분 대신 명시적인 25분 상한을 사용합니다.
# Image practice ownership

Use `ckad image build --problem Q1 --context . --tag junkloud.local/app:practice`
to build the Dockerfile in the problem's current attempt. Context paths are
relative to that attempt, regardless of the terminal's working directory.
The command uses Docker with a five-minute limit, native Linux platform,
no build network and locally provided pinned bases (or `scratch`). A context
is limited to 32 MiB and 4096 regular files/directories; symlinks are rejected.
This first slice supports one FROM stage and local COPY; ADD, external stages,
remote frontends and RUN isolation overrides are explicit errors.

Managed builds record intent before Docker effects and attach environment,
session, problem and build-operation labels. The exact tag and image ID are
saved. Rebuilding the same owned tag removes its proven prior ID before the
new build; a failed replacement therefore needs a fresh build after recovery.
Foreign or shared tags are preserved. Interrupted builds block new starts;
`ckad status` shows the intent and `ckad reset --recover` reconciles only that
record. A moved/missing reference whose ID remains, foreign labels or shared
tags retain the operation and report an ownership error. No prune or forced
image deletion is used. Personal Dockerfiles, archives and old attempts stay.

Ordinary `docker build` images can pass the same functional criteria without
JunKloud labels. Declared image references are observed at grading and reset,
so automatic reset reports a retained ownership error for an unproven image.
Observations are stored separately and never grant deletion authority: cleanup
also requires that exact ID/reference in the matching durable build operation.
After inspecting and explicitly removing that image yourself, run
`ckad reset --recover`. Use the managed command to obtain automatic owned
cleanup. Save the required archive with ordinary
`docker save --output image.tar junkloud.local/app:practice`.

Author solution/case actions support `file {source,path}` and
`image {operation: build|save,path,reference}` in the staged grader workspace.
Sources must be private setup/solution/cases package paths. All actions are
preflighted before writes; initial and reset assets stay original. File/image
setup actions remain unsupported. Bounded Helm install and Kustomize
solution/case actions are supported as described in
[author-tool-actions.md](../docs/author-tool-actions.md); tool setup and arbitrary
patch actions remain unsupported.
Save outputs reserve regular-file paths during preflight; building a context
that depends on an earlier save output is explicitly unsupported.
Native image export uses the platform-selecting command. Only Docker's exact
pre-API-1.48 unsupported-platform error enables legacy save, after immutable
ID, unchanged tag and Linux/native-architecture checks; all other errors fail.
