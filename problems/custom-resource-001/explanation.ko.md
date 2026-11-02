## 개념
CustomResourceDefinition(CRD)은 클러스터에 새로운 API 리소스 타입을 추가하는 오브젝트입니다. 관리자가 CRD를 설치하면 `kubectl`로 그 타입의 오브젝트(커스텀 리소스)를 Deployment처럼 만들고 조회할 수 있습니다. 이 문제의 CRD는 `backups.ops.junkloud.dev`입니다. 그룹은 `ops.junkloud.dev`, 버전은 `v1`, kind는 `Backup`, 네임스페이스 범위이며 짧은 이름은 `bk`입니다. CRD의 `openAPIV3Schema`는 API 서버가 커스텀 리소스를 저장하기 전에 검증하는 규칙입니다. 이 스키마는 `spec.schedule`(문자열), `spec.target`(`kind`는 `Deployment` 또는 `StatefulSet`, 그리고 `name`), `spec.retentionDays`(1~30 정수)를 필수로 요구하고, `spec.description`(문자열)은 선택입니다. 필수 필드가 없거나 범위를 벗어나면 API 서버가 생성을 거부합니다.

## 진단 순서
1. `kubectl get crd`나 `kubectl api-resources`로 클러스터에 설치된 커스텀 타입을 찾습니다. 백업과 관련된 타입은 `backups.ops.junkloud.dev` 하나입니다. `kubectl api-resources --api-group=ops.junkloud.dev`는 그룹, 버전, 짧은 이름, 네임스페이스 범위까지 보여 줍니다.
2. `kubectl explain backup.spec`(또는 `kubectl get crd backups.ops.junkloud.dev -o yaml`)으로 필수 필드와 허용 값을 확인합니다. `apiVersion`은 `<group>/<version>`, 즉 `ops.junkloud.dev/v1`입니다.
3. 매니페스트를 작성해 `kubectl apply -f`로 적용하고 `kubectl get backups -n <namespace>`(또는 `kubectl get bk`)로 확인합니다.

## 해결
- `answers/backup-crd.txt`에 `backups.ops.junkloud.dev`를 한 줄로 적습니다. 앞뒤 공백과 빈 줄은 무시됩니다.
- 다음 오브젝트를 `${namespace}`에 만듭니다.
  ```yaml
  apiVersion: ops.junkloud.dev/v1
  kind: Backup
  metadata:
    name: nightly
  spec:
    schedule: "0 2 * * *"
    target:
      kind: Deployment
      name: ledger
    retentionDays: 7
  ```
- 같은 값을 가진 ConfigMap `nightly`를 만들어도 `Backup` 타입이 아니므로 `backup-exists`부터 점수를 얻지 못합니다. `spec.description` 같은 스키마에 선언된 선택 필드를 추가하거나 필드 순서를 바꾸는 것은 허용됩니다.

## 채점
- `crd-discovered`: `answers/backup-crd.txt`의 한 줄이 `backups.ops.junkloud.dev`여야 합니다. 앞뒤 공백은 무시하며, `kubectl`을 실행한 이력은 보지 않습니다.
- `backup-exists`: `ops.junkloud.dev/v1`의 `Backup` `nightly`가 `${namespace}`에 있어야 합니다.
- `backup-schedule`, `backup-target`, `backup-retention`: 각각 `spec.schedule`이 `0 2 * * *`, `spec.target`이 `Deployment`/`ledger`, `spec.retentionDays`가 7이어야 합니다. `Backup`이 없으면 이 세 기준은 "선행 조건 미충족"으로 FAIL입니다.
- CRD는 관리자가 제공하며 이 문제의 소유 범위(scope)에 선언되어 있습니다. reset을 하면 CRD가 지워지고 다시 설치되며, CRD가 없는 상태에서 채점해도 `Backup`을 "찾을 수 없음"으로 FAIL 처리할 뿐 ERROR가 아닙니다. 학습자가 만든 클러스터 범위 오브젝트는 reset으로 지워지지 않으므로, 이 문제에서는 네임스페이스 오브젝트만 사용합니다.

## 공식 문서
- [Custom Resources](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/)
- [Extend the Kubernetes API with CustomResourceDefinitions](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/)
- [kubectl api-resources](https://kubernetes.io/docs/reference/kubectl/generated/kubectl_api-resources/)
