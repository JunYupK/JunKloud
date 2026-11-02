## 개념
ConfigMap은 평문 설정을, Secret은 토큰·비밀번호 같은 민감한 값을 Pod 밖에 보관합니다. 컨테이너는 `env[].valueFrom.configMapKeyRef` 또는 `secretKeyRef`로 키 하나를 환경변수로 받습니다. 값을 Deployment에 `value:`로 직접 적으면 설정이 코드에 굳고, 특히 토큰이 매니페스트와 `kubectl get deploy -o yaml` 출력에 그대로 남습니다. 참조한 키가 존재하지 않으면(`optional: true`가 아닌 한) kubelet이 컨테이너를 만들지 못해 Pod는 `CreateContainerConfigError` 상태에 머뭅니다. 키 이름은 대소문자를 구분하며 `api_token`과 `api-token`, `currency`와 `Currency`는 서로 다른 키입니다.

## 진단 순서
1. `kubectl get pods -n <namespace>`에서 STATUS가 `CreateContainerConfigError`인지 확인합니다 (이미지나 프로세스 문제가 아니라 컨테이너 생성 단계에서 막힌 것입니다).
2. `kubectl describe pod`의 Events에서 `couldn't find key api_token in Secret ...` 같은 메시지로 어느 참조가 틀렸는지 읽습니다.
3. `kubectl get configmap shop-config -o yaml`로 키 이름을 확인하고, `kubectl describe secret shop-secret`으로 Secret의 키 이름만 확인합니다 (describe는 값을 출력하지 않습니다).
4. `kubectl edit deployment shop` 또는 manifest 수정으로 env를 고칩니다. 이미 있는 `value: USD`는 `valueFrom`으로 바꾸려면 `value`를 지워야 합니다 (둘을 함께 둘 수 없습니다).
5. `kubectl rollout status deployment/shop`과 Service 응답으로 확인합니다.

## 해결
- `CURRENCY`: `valueFrom.configMapKeyRef`에 `name: shop-config`, `key: currency`를 적습니다. 기존의 literal `value: USD`는 제거합니다.
- `API_TOKEN`: `valueFrom.secretKeyRef`의 `key`를 `api_token`에서 `api-token`으로 고칩니다 (`name: shop-secret`).
- 앱은 `$API_TOKEN`의 sha256 해시를 내장된 해시와 비교해 `token=ok` 또는 `token=bad`를 출력할 뿐 토큰 자체를 출력하지 않으므로, 응답의 `token=ok`가 Secret 값이 올바르게 전달되었다는 증거입니다. ConfigMap 값 `EUR`을 `shop currency=EUR`로 확인합니다.
- ConfigMap의 값을 바꿔 출력을 맞추는 것은 해법이 아닙니다. 고쳐야 할 곳은 Deployment의 참조입니다.

## 오답 피드백
- `hardcoded-values`: env에 `EUR`와 토큰을 `value:`로 직접 적으면 앱은 `shop currency=EUR token=ok`를 반환하고 Pod도 Ready이므로 `shop-ready`, `shop-serves`, `inputs-unchanged`는 얻지만, 참조가 아니므로 `currency-ref`와 `token-ref`를 잃습니다 (3점). 동작해도 이 방식은 설정과 비밀을 Deployment에 굳혀 두는 안티패턴입니다. 채점 결과(기대·관찰 값, 사유)에는 토큰이 나타나지 않으며 `token-ref`의 관찰 값은 참조 경로가 비어 있다는 사실뿐입니다.
- `wrong-configmap-key`: `configMapKeyRef.key`를 `Currency`로 쓰면 ConfigMap에 그 키가 없어 컨테이너가 시작되지 않고(`CreateContainerConfigError`) `currency-ref`를 잃습니다. Pod가 Ready가 되지 않으므로 `shop-ready`도 잃고 이에 의존하는 `shop-serves`도 잃습니다 (2점). 컨테이너가 시작되지 못하는 것은 학습자의 설정 오류이므로 환경 오류(ERROR)가 아니라 FAIL입니다.
- `configmap-edited`: 참조는 맞지만 ConfigMap의 `currency`를 `GBP`로 바꾸면 응답이 `shop currency=GBP ...`가 되어 `shop-serves`를 잃고, ConfigMap이 바뀌었으므로 `inputs-unchanged`도 잃습니다 (3점).
- `optional: false`를 명시하거나 `LOG_LEVEL` 같은 관련 없는 환경변수를 추가해도 참조의 `name`과 `key`만 검사하므로 만점입니다 (5점).

## 채점
5개 기준을 각각 1점으로 채점합니다. `currency-ref`와 `token-ref`는 Deployment의 컨테이너 `app`에서 환경변수 이름으로 항목을 찾아 `valueFrom`의 참조 `name`과 `key`를 비교하며 `optional` 값과 다른 환경변수는 보지 않습니다. `shop-ready`는 replica 1개가 10초 안정 구간(관찰 한도 90초) 동안 Ready여야 합니다. `shop-serves`는 `shop-ready`가 통과해야 평가하며 Service 80번 포트가 최대 60초 안에 `shop currency=EUR token=ok`를 반환해야 합니다. `inputs-unchanged`는 ConfigMap `shop-config`의 `data.currency`가 `EUR`인지 확인합니다. 모든 채점 증거와 출력은 Secret 값을 포함하지 않습니다. 실행한 명령어 이력은 채점하지 않습니다.

## 공식 문서
- [ConfigMaps](https://kubernetes.io/docs/concepts/configuration/configmap/)
- [Secrets](https://kubernetes.io/docs/concepts/configuration/secret/)
- [Define Environment Variables for a Container](https://kubernetes.io/docs/tasks/inject-data-application/define-environment-variable-container/)
- [Distribute Credentials Securely Using Secrets](https://kubernetes.io/docs/tasks/inject-data-application/distribute-credentials-secure/)
