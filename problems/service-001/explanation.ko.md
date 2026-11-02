## 개념
Service는 Pod를 직접 가리키지 않고 `selector`로 라벨이 맞는 Pod를 고릅니다. 컨트롤 플레인은 이렇게 고른 Pod의 IP와 포트를 EndpointSlice에 기록합니다. Ready가 아닌 Pod도 `conditions.ready=false`로 목록에 남지만, kube-proxy는 `ready=true`인 주소만 사용해 Service의 `port`로 들어온 요청을 보냅니다. 이때 Pod 쪽 포트는 Service의 `targetPort`가 정합니다. `targetPort`에는 숫자(`8080`)를 쓰거나 컨테이너 포트의 이름(`http`)을 쓸 수 있으며, 이름을 쓰면 Pod가 그 이름의 포트를 어느 번호로 열었는지를 따라갑니다. 그래서 Service가 안 되는 원인은 대개 둘 중 하나입니다. selector가 Pod 라벨과 맞지 않아 EndpointSlice가 비어 있거나, `targetPort`가 앱이 실제로 listen하는 포트와 달라 연결이 거부됩니다.

Service `type`은 접근 범위를 정합니다. `ClusterIP`(기본값)는 클러스터 안에서만, `NodePort`는 ClusterIP에 더해 모든 노드의 같은 포트(기본 범위 30000-32767)로도 열립니다. `nodePort`를 지정하지 않으면 범위에서 임의로 할당되므로, 값이 정해진 문제에서는 `nodePort: 30080`을 직접 적어야 합니다.

## 진단 순서
1. `kubectl get svc catalog -o wide`로 Service의 type, 포트, selector를 봅니다.
2. `kubectl get endpointslices -l kubernetes.io/service-name=catalog`로 Service가 고른 Pod 주소가 있는지 확인합니다(예전 `kubectl get endpoints`는 deprecated 입니다). 비어 있으면 selector가 문제이고, 주소가 있어도 `conditions.ready`가 `false`이면 kube-proxy가 쓰지 않는 주소입니다.
3. `kubectl get pods --show-labels`로 Pod의 라벨을 확인하고 Service의 selector와 비교합니다. Pod가 `app=catalog`, `tier=backend` 라벨을 가졌는데 Service는 `app=catalog-api`를 찾고 있습니다.
4. 주소가 있어도 연결이 안 되면 `kubectl get pod -o jsonpath` 또는 `kubectl describe pod`로 컨테이너 포트(8080, 이름 `http`)를 확인하고 Service의 `targetPort`와 비교합니다. 여기서는 `9090`이라 연결이 거부됩니다.
5. `kubectl logs deploy/frontend --tail=3`으로 frontend가 `frontend fetch failed`를 찍는지, 고친 뒤 `frontend fetched catalog-ok`를 찍는지 봅니다.

## 해결
- Service `catalog`의 selector를 Pod 라벨에 맞춥니다(`app: catalog`; `tier: backend`를 함께 써도 됩니다). 채점은 selector의 `app` 값이 `catalog`인지 확인합니다.
- Pod 라벨을 Service에 맞추는 길은 해결이 아닙니다. Deployment의 Pod template을 바꾸면 `workloads-unchanged`를 잃고, 실행 중인 Pod에 `kubectl label pods -l app=catalog app=catalog-api --overwrite`처럼 라벨을 직접 붙이면 ReplicaSet이 그 Pod를 놓아주고(고아) 새 Pod를 만들기 때문에 `catalog-ready`와 `workloads-unchanged`는 통과하고 고아 Pod 주소로 `service-routes`도 통과합니다. 그래서 selector의 `app` 값은 `service-shape`가 따로 확인하며, 오래된 selector `app: catalog-api`를 그대로 두면 이 점수를 잃습니다.
- `targetPort`를 `8080`(또는 컨테이너 포트 이름 `http`)으로 고치고 port 이름은 `http`로 둡니다.
- `type: NodePort`, `nodePort: 30080`을 지정합니다. 예: `kubectl patch svc catalog --type merge -p '{"spec":{"type":"NodePort","selector":{"app":"catalog"},"ports":[{"name":"http","port":80,"targetPort":8080,"nodePort":30080}]}}'`.
- `frontend`는 3초마다 스스로 다시 요청하므로 Service를 고치면 재시작 없이 몇 초 뒤 `frontend fetched catalog-ok`를 찍습니다.

## 오답 피드백
- `pods-relabeled`: Pod 라벨을 Service의 `app: catalog-api`에 맞추면 EndpointSlice는 채워지지만, Deployment의 selector를 바꿀 수 없으므로 Deployment를 지우고 다시 만들어야 하고 그 결과 `workloads-unchanged`를 잃습니다. 또 `targetPort`가 그대로라 `service-routes`, `service-shape`, `frontend-reaches`도 통과하지 못합니다 (`catalog-ready`와 `frontend-ready` 2점).
- `wrong-target-port`: selector와 NodePort는 맞췄지만 `targetPort`가 `9090`이면 요청이 거부되어 `service-routes`와 `frontend-reaches`를 잃습니다 (4점).
- `clusterip-only`: 라우팅은 고쳤지만 type이 `ClusterIP`이면 `service-shape`를 잃습니다 (5점).
- `empty-service`: selector를 지운 Service는 어떤 Pod도 고르지 않아 EndpointSlice가 비고, NodePort를 맞춰도 `service-routes`, `service-shape`, `frontend-reaches`를 잃습니다 (3점). selector를 비우면 Service가 아무 곳으로도 보내지 않는다는 점을 확인하세요.
- `pods-labelled-directly`: 오래된 selector `app: catalog-api`를 그대로 두고 그 라벨을 가진 Pod가 따로 떠 있으면(`targetPort`와 NodePort를 고친 상태) 요청은 그 Pod에 도달해 `service-routes`와 `frontend-reaches`를 얻지만, selector의 `app` 값이 `catalog`가 아니므로 `service-shape`를 잃습니다 (5점).
- `alternative-named-target-port`: `targetPort: http`(이름)와 `app: catalog`만 쓰는 selector도 같은 Pod와 같은 포트에 도달하므로 만점입니다 (6점).

## 채점
6개 기준을 각각 1점으로 채점합니다. `catalog-ready`와 `frontend-ready`는 replica가 Ready여야 하며(10초 안정 구간, 관찰 한도 90초) 이 둘이 통과해야 뒤의 기준이 평가됩니다. `workloads-unchanged`는 catalog Pod template의 라벨 `app`·`tier`, 이미지, command가 제공된 값 그대로인지 확인합니다. `service-routes`는 채점기가 클러스터 안에서 Service `catalog`의 80번 포트로 실제 HTTP 요청을 보내 본문에 `catalog-ok`가 있는지(60초 한도) 보며, `catalog-ready`가 통과해야 평가합니다. 그래서 `targetPort`를 번호로 쓰든 이름으로 쓰든 8080에 도달하면 인정되고, selector가 어떤 Pod를 고르는지는 `service-shape`가 확인합니다. `service-shape`는 `spec.selector.app`이 `catalog`이고 `spec.type`이 `NodePort`이며 이름이 `http`인 포트의 `nodePort`가 `30080`인지 확인합니다. `frontend-reaches`는 frontend의 로그에서 `frontend fetched catalog-ok` 줄을 60초까지 기다려 찾으며 `service-routes`와 `frontend-ready`가 통과해야 평가합니다. 실행한 명령어 이력은 채점하지 않습니다.

## 공식 문서
- [Service](https://kubernetes.io/docs/concepts/services-networking/service/)
- [Connecting Applications with Services](https://kubernetes.io/docs/tutorials/services/connect-applications-service/)
- [Debug Services](https://kubernetes.io/docs/tasks/debug/debug-application/debug-service/)
- [EndpointSlices](https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/)
