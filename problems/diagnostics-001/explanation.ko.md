## 개념
Pod가 시작하자마자 종료하는 CrashLoopBackOff는 "컨테이너가 왜 종료했는가"를 읽어야 풀립니다. 이 문제의 `orders` 앱은 ConfigMap을 볼륨으로 마운트해 `/etc/orders/app.conf` 파일을 읽는데, ConfigMap에는 키 이름이 `orders.conf`로 들어 있어 파일이 없습니다. 앱은 종료하기 전에 로그에 원인(`FATAL: /etc/orders/app.conf not found (mounted: orders.conf)`)을 남깁니다. 또한 `kubectl top`은 metrics-server가 수집한 CPU/메모리 사용량을 보여 주므로, 네임스페이스에서 CPU를 가장 많이 쓰는 Pod를 찾는 데 쓰입니다.

## 진단 순서
1. `kubectl get pods -n <namespace>`로 `orders` Pod의 STATUS가 `CrashLoopBackOff`이고 RESTARTS가 늘어나는지 봅니다.
2. `kubectl logs deploy/orders -n <namespace>`(필요하면 `--previous`)로 종료 직전 메시지를 읽습니다. 앱이 기대하는 파일 이름과 실제로 마운트된 파일 이름(`mounted: orders.conf`)이 다르다는 것을 알 수 있습니다.
3. `kubectl describe pod`와 `kubectl get configmap orders-config -o yaml`로 마운트 경로와 ConfigMap 키를 확인합니다.
4. 원인을 고친 뒤 `kubectl rollout restart deploy/orders`로 Pod를 새로 만들고 `kubectl rollout status`, `kubectl get endpoints orders`로 확인합니다.
5. `kubectl top pod -n <namespace>`로 CPU 사용량이 가장 큰 Pod를 찾습니다. metrics-server는 수집 주기가 있어 Pod 생성 직후에는 값이 비어 있을 수 있으니 잠시 후 다시 실행합니다.

## 해결
- 앱의 command는 바꾸지 않고 설정을 고칩니다. ConfigMap `orders-config`에 키 `app.conf`(값은 기존 `endpoint=orders-db.<namespace>.svc:5432`와 동일)를 추가하거나, Deployment 볼륨에 `items: [{key: orders.conf, path: app.conf}]`를 지정해 같은 파일을 `app.conf`라는 이름으로 노출해도 됩니다. 두 방법 모두 인정됩니다.
- 이미 크래시한 Pod는 CrashLoopBackOff 대기와 ConfigMap 볼륨 갱신 주기 때문에 늦게 회복될 수 있으므로 `kubectl rollout restart deploy/orders`로 새 Pod를 만듭니다. (ConfigMap에 키를 추가하면 기존 `orders.conf` 키는 그대로 남지만, 앱은 `app.conf`만 읽으므로 문제가 없습니다.)
- `kubectl top pod -n <namespace>` 결과에서 CPU가 가장 큰 Pod(`report-worker`는 busy loop로 CPU limit 200m 가까이를 씁니다)의 이름을 `answers/top-cpu.txt`에 한 줄로 적습니다.

## 채점
- `top-cpu-answer`: 파일 내용이 `report-worker` 한 줄이어야 합니다.
- `command-unchanged`: `app` 컨테이너의 image와 command가 처음 제공된 값 그대로여야 합니다. command를 `sleep 3600`으로 바꾸면 Pod는 Ready처럼 보이지만 앱 로그와 Service 응답이 없고, command에 endpoint를 직접 적어 넣으면 겉으로는 동작해도 원인을 고친 것이 아니므로 이 점수를 얻지 못합니다.
- `orders-ready`, `orders-started`, `service-serves`: 새 Pod가 Ready가 되고, 시작 로그 `orders started endpoint=orders-db.`가 보이며, Service가 `orders-ok`를 반환해야 합니다. 설정 파일이 없어서 앱이 종료하는 것은 학습자의 오류이므로 환경 오류(ERROR)가 아니라 FAIL입니다.
- 실행한 명령어 이력은 채점하지 않으며, `kubectl top` 사용 여부도 보지 않고 답 파일의 내용만 확인합니다.

## 공식 문서
- [Debug Running Pods](https://kubernetes.io/docs/tasks/debug/debug-application/debug-running-pod/)
- [Determine the Reason for Pod Failure](https://kubernetes.io/docs/tasks/debug/debug-application/determine-reason-pod-failure/)
- [Resource metrics pipeline (kubectl top)](https://kubernetes.io/docs/tasks/debug/debug-cluster/resource-metrics-pipeline/)
- [Configure a Pod to Use a ConfigMap](https://kubernetes.io/docs/tasks/configure-pod-container/configure-pod-configmap/)
