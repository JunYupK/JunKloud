## 개념
NetworkPolicy는 Pod 단위의 방화벽 규칙입니다. 정책의 `podSelector`가 고른 Pod는 그 정책의 `policyTypes`(`Ingress`, `Egress`) 방향에서 "격리"되어, 어떤 정책이 허용한 연결만 통과합니다. 같은 Pod를 고르는 정책이 여럿이면 허용 규칙의 합집합이 적용되고, 어떤 정책도 고르지 않은 Pod는 모든 연결을 받습니다. 그래서 api의 ingress 정책을 하나라도 만들면 api는 격리되어 허용한 곳 외의 연결이 모두 막히고, web의 egress 정책을 만들면 web이 나가는 연결은 허용한 목적지로만 갈 수 있습니다. 거부는 거부 응답이 아니라 패킷을 버리는 방식이어서 막힌 클라이언트는 연결 시간 초과(timeout)를 봅니다. 연결 거부(refused)는 정책이 아니라 그 포트에서 아무것도 듣지 않는 서버의 반응입니다.

`from`과 `to`의 항목 하나 안에 `namespaceSelector`와 `podSelector`를 함께 쓰면 AND이고(그 네임스페이스에 있고 그 라벨을 가진 Pod), 항목을 나누어 쓰면 OR입니다. `podSelector`만 쓰면 정책이 있는 네임스페이스의 Pod만 가리킵니다. 다른 네임스페이스는 `namespaceSelector`로 고르며, 모든 네임스페이스에 자동으로 붙는 라벨 `kubernetes.io/metadata.name`을 쓰면 이름으로 고를 수 있습니다. egress를 제한하면 DNS 질의도 막히므로 Service 이름을 쓰는 클라이언트는 kube-system의 DNS(UDP와 TCP 53번 포트)를 허용해야 합니다. egress 정책은 Service의 ClusterIP가 아니라 DNAT 후의 Pod 주소와 포트(여기서는 api의 8080번 포트)를 기준으로 평가됩니다.

## 진단 순서
1. `kubectl get pods --show-labels -n <namespace>`와 `kubectl get pods --show-labels -n <namespace>-mon`으로 api(`role=api`), web(`role=web`), batch(`role=batch`)와 monitor(`app=monitor`), intruder(`component=intruder`)의 라벨을 확인합니다. 정책은 이 라벨로 Pod를 고릅니다.
2. `kubectl get networkpolicy`와 `kubectl describe networkpolicy <이름>`으로 각 정책의 `podSelector`, `policyTypes`, 허용 규칙을 봅니다. 정책이 `Ingress`만 가지는지 `Egress`도 가지는지, 어떤 Pod를 격리하는지 확인합니다.
3. 요구를 정책으로 나눕니다. api의 ingress는 role=web(같은 네임스페이스)과 app=monitor(`<namespace>-mon` 네임스페이스에서 AND)만 허용하고 포트는 8080입니다. web의 egress는 api의 8080번 포트와 kube-system의 53번 포트(UDP, TCP)입니다.
4. 적용한 뒤 허용된 클라이언트로 먼저 확인하고(예: web에서 `api`에 요청), 막힌 클라이언트가 응답 없이 시간 초과가 되는지 봅니다. 모두 막혀 있다면 허용 규칙이나 DNS를 빠뜨린 것입니다.

## 해결
정책 세 개를 `<namespace>`에 만듭니다(이름은 자유입니다).
```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: api-from-web
spec:
  podSelector:
    matchLabels:
      role: api
  policyTypes: [Ingress]
  ingress:
  - from:
    - podSelector:
        matchLabels:
          role: web
    ports:
    - {protocol: TCP, port: 8080}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: api-from-monitor
spec:
  podSelector:
    matchLabels:
      role: api
  policyTypes: [Ingress]
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: <namespace>-mon
      podSelector:
        matchLabels:
          app: monitor
    ports:
    - {protocol: TCP, port: 8080}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: web-egress
spec:
  podSelector:
    matchLabels:
      role: web
  policyTypes: [Egress]
  egress:
  - to:
    - podSelector:
        matchLabels:
          role: api
    ports:
    - {protocol: TCP, port: 8080}
  - to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: kube-system
    ports:
    - {protocol: UDP, port: 53}
    - {protocol: TCP, port: 53}
```
- 허용 규칙을 나눠 정책 두 개로 써도, 항목 두 개의 한 정책으로 합쳐도 같은 결과입니다(`alternative-single-policy`). 합칠 때도 monitor 항목은 `namespaceSelector`와 `podSelector`를 한 항목 안에 함께 둡니다.
- Deployment의 라벨을 바꾸지 않습니다. 채점기의 검사용 Pod는 문제에 적힌 라벨을 직접 달고 실행되므로 Deployment를 바꿔도 연결 결과가 달라지지 않고, `workloads-unchanged` 점수만 잃습니다.
- 채점은 정책 객체의 모양이 아니라 연결 결과를 봅니다. 허용된 클라이언트가 같은 Pod에 연결되는 동안 막힌 클라이언트가 시간 초과가 되어야 막힘으로 인정합니다. 서버가 죽었거나 모두를 막는 상태는 정답이 아닙니다.

## 오답 피드백
- `deny-all`: 모든 Pod의 ingress와 egress를 막는 `default-deny` 하나만 만들면 허용 규칙이 없어서 web이 DNS도 api도 쓰지 못해 `web-reaches-api`를 잃습니다. 같은 네임스페이스에서 허용되어야 할 클라이언트(web, batch)는 egress가 막혀 Service 이름을 찾는 단계(DNS)에서 실패하고, `<namespace>-mon`의 monitor와 비교용 클라이언트는 이름은 찾지만 api로의 연결이 시간 초과가 됩니다. 허용되어야 할 클라이언트가 접속하지 못하므로 `batch-blocked`, `monitor-allowed`, `unlabeled-blocked`, `web-egress-blocked`도 잃습니다 (3점: `api-ready`, `ledger-ready`, `workloads-unchanged`). 막힘은 허용된 경로가 살아 있을 때만, 그리고 이름을 찾은 뒤의 시간 초과일 때만 인정됩니다.
- `allow-all-ingress`: api의 ingress를 `ingress: [{}]`로 모든 곳에 열면 web과 monitor는 통과하고 egress도 맞지만, batch, intruder, 라벨 없는 Pod가 모두 연결되어 `batch-blocked`, `monitor-allowed`, `unlabeled-blocked`를 잃습니다 (5점).
- `batch-relabeled`: 정책은 정답인데 batch Pod의 라벨을 `role: web`으로 바꾸면 `workloads-unchanged` 한 점만 잃습니다 (7점). 검사용 Pod는 선언된 라벨을 달고 실행되므로 라벨을 바꿔도 정책 우회가 되지 않습니다.
- `missing-egress`: ingress 정책만 만들고 web의 egress를 제한하지 않으면 web이 ledger에 연결되어 `web-egress-blocked`를 잃습니다 (7점).
- `and-or-split`: monitor를 허용하는 `from`을 항목 두 개로 나누어 `namespaceSelector`(`<namespace>-mon`)와 `podSelector`(`app: monitor`)를 따로 쓰면 OR이 되어, `<namespace>-mon`의 모든 Pod(intruder 포함)와 `<namespace>`의 `app: monitor` Pod가 모두 허용됩니다. 두 개의 막힘이 깨지므로 `monitor-allowed`만 잃습니다 (7점). 한 항목 안에 두 selector를 함께 써야 AND입니다.
- `alternative-single-policy`: 하나의 정책에 `from` 항목 두 개(role=web, 그리고 `<namespace>-mon`의 app=monitor)를 쓰고, 별도의 egress 정책이 api를 `app: api`로, DNS를 kube-system의 `k8s-app: kube-dns` Pod로 고르는 방식도 같은 연결 결과를 만들므로 만점입니다 (8점).

## 채점
8개 기준을 각각 1점으로 채점합니다. `api-ready`와 `ledger-ready`는 replica 1개가 Ready여야 하며(10초 안정 구간, 관찰 한도 90초) `api-ready`는 `web-reaches-api`, `batch-blocked`, `monitor-allowed`, `unlabeled-blocked`(기준 3~6)가 평가되기 위한 전제이고, `ledger-ready`는 `web-egress-blocked`(기준 7)의 전제입니다. `workloads-unchanged`는 전제가 없습니다. `web-reaches-api`는 role=web 클라이언트가 Service api의 80번 포트로 요청해 본문 `api-ok`를 받는지 보며 DNS와 egress가 모두 맞아야 통과합니다. `batch-blocked`는 role=web 클라이언트는 접속하고 role=batch 클라이언트는 막히는지, `monitor-allowed`는 `<namespace>-mon`의 app=monitor는 접속하고 같은 네임스페이스의 intruder와 `<namespace>`의 app=monitor는 막히는지, `unlabeled-blocked`는 role=web은 접속하고 라벨 없는 Pod는 막히는지 확인합니다. `web-egress-blocked`는 role=batch가 Service ledger의 `ledger-ok`를 받고 role=web이 막히는지 확인합니다. 막힘은 Service 이름이 아니라 준비된 Pod 주소마다 판정하며, 허용된 클라이언트가 같은 Pod에 접속하는 동안 막힌 클라이언트가 timeout이어야 하고(60초 한도), 이름을 찾지 못하는 것은 막힘이 아닙니다. `workloads-unchanged`는 batch Pod template의 라벨, 이미지, command가 제공된 값 그대로인지 확인합니다. 실행한 명령어 이력은 채점하지 않습니다.

## 공식 문서
- [Network Policies](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- [Declare Network Policy](https://kubernetes.io/docs/tasks/administer-cluster/declare-network-policy/)
- [DNS for Services and Pods](https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/)
