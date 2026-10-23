## 개념
Kubernetes에는 세 가지 probe가 있습니다. readinessProbe는 Pod가 트래픽을 받을 준비가 되었는지 판단하며 실패하면 Service endpoint에서 빠집니다. livenessProbe는 컨테이너가 살아 있는지 판단하며 실패하면 kubelet이 컨테이너를 재시작합니다. startupProbe는 느리게 시작하는 앱을 위해 성공할 때까지 readiness/liveness 검사를 미룹니다. 시작이 느린 앱에 liveness만 짧게 걸면 시작도 끝나기 전에 재시작을 반복합니다.

## 진단 순서
1. `kubectl get pods -n <namespace>`로 RESTARTS가 늘고 READY가 0/1인지 확인합니다.
2. `kubectl describe pod`의 Events에서 `Liveness probe failed`, `Readiness probe failed` 메시지와 실패한 경로/포트를 읽습니다.
3. 앱이 실제로 듣는 포트와 제공하는 경로(`/ready`, `/healthz`)를 확인하고, 시작에 걸리는 시간(약 10초)을 계산합니다.
4. `kubectl edit deploy checkout` 또는 manifest 수정 후 apply로 probe를 고치고, `kubectl rollout status`와 `kubectl get endpointslices -l kubernetes.io/service-name=checkout`으로 확인합니다.

## 해결
- readinessProbe: `httpGet` path `/ready`, port를 앱 포트(8080 또는 컨테이너 포트 이름)로 맞춥니다. 8081처럼 아무도 듣지 않는 포트는 영원히 Ready가 되지 않습니다.
- livenessProbe: path `/healthz`로 고칩니다. 존재하지 않는 경로(`/health`)는 앱이 뜬 뒤에는 404로 실패합니다. 다만 이 앱은 시작에 약 10초가 걸리고 startupProbe가 없어서, 처음 liveness 검사는 아직 아무도 듣지 않는 포트에서 `connection refused`로 실패하고 `failureThreshold: 1`이라 컨테이너가 앱이 뜨기도 전에 재시작됩니다. 그래서 `describe pod`의 Events에서는 404보다 `connection refused`가 먼저 보입니다. 경로를 고쳐도 startupProbe 없이 시작 지연을 넘기지 못하면 재시작이 계속됩니다. 정답 manifest는 `failureThreshold: 3`을 써서 느린 호스트에서 한 번 늦은 응답으로 컨테이너가 죽지 않도록 합니다.
- startupProbe: path `/healthz`, 예를 들어 `periodSeconds: 2`, `failureThreshold: 15`처럼 `periodSeconds x failureThreshold`가 시작 시간(약 10초)보다 길게 설정합니다. 성공하기 전에는 liveness가 시작되지 않으므로 재시작 루프가 사라집니다. liveness에 `initialDelaySeconds`만 늘리는 방법은 시작 시간이 달라지면 다시 깨지므로 startupProbe가 권장됩니다.

## 채점
probe 경로 3개를 각각 1점으로 채점하고(startupProbe 점수는 `image`와 `command`가 제공된 그대로일 때만 인정되므로 시작 지연을 command에서 지워 startupProbe 없이 통과하는 우회는 이 점수를 얻지 못합니다), 포트는 값이 아니라 동작으로 검증합니다. 그래서 `port: 8080`과 컨테이너 포트 이름(`http`)은 모두 인정됩니다. 두 replica가 40초 안정 구간(관찰 한도 120초) 동안 재시작 없이 Ready/Available이어야 `rollout-stable`을 얻고, Service가 `checkout-ok`를 반환해야 `service-serves`를 얻습니다. 잘못된 probe 때문에 앱이 실패하는 것은 학습자의 오류로 FAIL이며 환경 오류(ERROR)가 아닙니다. probe를 모두 지우면 Pod는 바로 Ready가 되고 Service도 결국 응답하므로 2점은 얻지만 세 probe 점수는 얻지 못합니다. readiness 포트를 8081로 두면 Ready가 되지 않아 rollout과 Service 점수를 잃습니다. liveness 포트를 잘못 지정하면(기본 period 10초 x failureThreshold 3) startupProbe 성공 약 20~30초 뒤에야 컨테이너가 재시작되므로, 잠깐 Ready처럼 보여도 40초 안정 구간 안에 재시작이 관찰되어 rollout과 Service 점수를 잃습니다(학습자 오류 FAIL이며 ERROR가 아닙니다). 실행한 명령어 이력은 채점하지 않으며 Pod를 임의로 삭제해 probe를 시험하지 않습니다.

## 공식 문서
- [Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- [Pod Lifecycle: Container probes](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#container-probes)
- [Debug Running Pods](https://kubernetes.io/docs/tasks/debug/debug-application/debug-running-pod/)
