## 개념
ResourceQuota는 네임스페이스가 쓸 수 있는 총량(`requests.cpu`, `limits.memory`, Pod 개수 등)을 제한합니다. 쿼터가 `requests.*`나 `limits.*`를 제한하면 그 자원을 지정하지 않은 Pod는 admission 단계에서 거부됩니다 (`must specify limits.cpu ...` 또는 `exceeded quota`). 이때 Pod 자체가 만들어지지 않으므로 Deployment의 ReplicaSet은 Pod 없이 `FailedCreate` 이벤트만 남기고, `kubectl get pods`는 비어 있습니다. LimitRange는 컨테이너에 자원을 적지 않았을 때 넣어 줄 기본값(`defaultRequest`, `default`)과 최소·최대값을 정하는 네임스페이스 정책입니다. 쿼터와 함께 쓰면 기본값 덕분에 자원을 적지 않은 Pod도 admission을 통과할 수 있습니다.

## 진단 순서
1. `kubectl get deploy,rs,pods -n <namespace>`로 Pod가 하나도 없는지 확인합니다 (Pod가 없으면 컨테이너가 아니라 생성 단계가 막힌 것입니다).
2. `kubectl describe rs -n <namespace>`의 Events에서 `forbidden: failed quota: team-quota: must specify ...` 메시지를 읽습니다.
3. `kubectl describe resourcequota team-quota`로 hard 한도와 현재 used를 확인하고, replica 수 x 컨테이너 요청량이 한도 안에 드는지 계산합니다 (2 x 100m = 200m은 500m 안, 2 x 300m = 600m은 500m 초과).
4. `kubectl set resources deployment/worker -c worker --requests=cpu=100m,memory=64Mi --limits=cpu=200m,memory=128Mi`로 고치고 `kubectl rollout status deployment/worker`로 확인합니다.
5. LimitRange `defaults`를 만들고 `kubectl describe limitrange defaults`로 기본값을 확인합니다.

## 해결
- Deployment `worker`의 컨테이너 `worker`에 requests `cpu: 100m`, `memory: 64Mi`, limits `cpu: 200m`, `memory: 128Mi`를 지정합니다. Pod 2개의 합계는 requests 200m / 128Mi, limits 400m / 256Mi로 쿼터 안입니다.
- LimitRange `defaults`에 `type: Container`, `defaultRequest`(`cpu: 50m`, `memory: 32Mi`), `default`(`cpu: 100m`, `memory: 64Mi`)를 적습니다.
- 쿼터를 지우거나 늘려서 통과하는 것은 해법이 아닙니다. 쿼터는 관리자가 정한 한도이며 이 문제는 `team-quota`를 그대로 두라고 요구합니다.

## 오답 피드백
- 쿼터를 삭제하면 Pod는 뜨지만 `team-quota`가 없으므로 `quota-intact`와 `quota-used`를 잃습니다 (3점).
- 쿼터의 `requests.cpu`를 2로 올리면 마찬가지로 `quota-intact`와 `quota-used`를 잃습니다 (3점).
- requests cpu를 300m로 두면 Pod 하나만 쿼터에 들어가고 (300m <= 500m) 두 번째 Pod는 500m를 넘어 거부됩니다. 값이 문제의 요구와 다르므로 `worker-resources`를 잃고, 이 값에 의존하는 `worker-ready`와 `quota-used`도 잃습니다 (2점). Pod 한 개가 먼저 admission을 통과해 실행되더라도 결과는 같습니다.
- LimitRange를 만들지 않으면 `limitrange-defaults`만 잃습니다 (4점).
- `cpu: 0.1`, `memory: 67108864`처럼 다른 표기로 같은 양을 적은 답은 정답입니다 (5점).

## 채점
5개 기준을 각각 1점으로 채점합니다. 수량은 표기가 아니라 값으로 비교하므로 `0.1`과 `100m`, `67108864`와 `64Mi`는 같습니다. `worker-ready`는 replica 2개가 10초 안정 구간(관찰 한도 90초) 동안 Ready여야 하며 `worker-resources`가 통과해야 평가합니다. 자원이 맞지 않아 Pod가 쿼터에 거부되면 Pod가 없는 상태이므로 이 기준은 환경 오류(ERROR)가 아니라 선행 조건 미충족으로 FAIL 처리됩니다. `quota-used`는 `worker-ready`와 `quota-intact`가 통과해야 평가하며 쿼터가 집계한 `requests.cpu` 사용량이 200m인지 최대 60초까지 확인합니다. 실행한 명령어 이력은 채점하지 않습니다.

## 공식 문서
- [Resource Quotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/)
- [Limit Ranges](https://kubernetes.io/docs/concepts/policy/limit-range/)
- [Resource Management for Pods and Containers](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/)
- [Configure Memory and CPU Quotas for a Namespace](https://kubernetes.io/docs/tasks/administer-cluster/manage-resources/quota-memory-cpu-namespace/)
