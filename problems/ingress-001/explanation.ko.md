## 개념
Ingress는 클러스터 밖에서 들어온 HTTP 요청을 `Host` 헤더와 경로에 따라 Service로 나누는 규칙입니다. 규칙만으로는 아무 일도 일어나지 않고, `spec.ingressClassName`이 가리키는 IngressClass의 controller가 규칙을 읽어 실제로 요청을 받아 전달합니다. 이 환경의 controller는 `cloud-provider-kind`이며 이 IngressClass에는 `ingressclass.kubernetes.io/is-default-class: "true"` annotation이 붙은 기본 class입니다. `ingressClassName`을 쓰지 않은 Ingress를 만들면 API server의 admission(DefaultIngressClass)이 기본 class 이름을 `spec.ingressClassName`에 채워 넣습니다. 반대로 다른 class(예: `nginx`)를 쓰면 이 환경의 controller는 그 Ingress를 맡지 않습니다. Ingress가 만들어지면 controller가 주소를 할당해 `kubectl get ingress`의 `ADDRESS` 칸에 채웁니다.

규칙의 선택 순서는 이렇습니다. 먼저 요청의 `Host`와 같은 `host`를 가진 rule이 고르고, 그 rule 안에서 요청 경로와 맞는 `path`가 고릅니다. `pathType: Prefix`는 경로를 `/`로 나눈 요소 단위의 접두어이므로 `/api`는 `/api`와 `/api/`와 `/api/v1`에 맞고 `/apix`에는 맞지 않습니다. 맞는 경로가 여럿이면 더 긴 경로가 이기므로 `/`와 `/api`가 한 host에 있어도 `/api/`는 `/api`로 갑니다. backend는 Service의 이름과 포트로 지정하며, 포트는 Service의 `port` 번호(`number: 80`) 또는 Service 포트의 이름(`name: http`)입니다. Pod의 `targetPort`(8080)가 아니라 Service가 여는 포트를 써야 합니다.

## 진단 순서
1. `kubectl get ingress shop`으로 `CLASS`, `HOSTS`, `ADDRESS`를 봅니다. `ADDRESS`가 비어 있으면 controller가 아직 이 Ingress를 맡지 않은 것입니다. `kubectl get ingressclass`로 사용할 수 있는 class 이름(`cloud-provider-kind`)을 확인합니다.
2. `kubectl describe ingress shop`의 Rules에서 host마다 경로가 어느 backend로 가는지 확인합니다. backend 옆에 `<error: ...>`가 보이면 Service 이름이나 포트가 틀린 것입니다.
3. `kubectl get svc,endpointslices`로 backend Service의 포트(80)와 endpoint가 있는지 확인합니다.
4. 주소가 나오면 host가 DNS에 없어도 `Host` 헤더를 직접 넣어 요청해 볼 수 있습니다. 예: `curl -H "Host: shop.<namespace>.example" http://<ADDRESS>/api/`. 본문의 `web-ok` 또는 `api-ok`가 어느 backend가 답했는지 알려 줍니다.

## 해결
- Ingress `shop`에 `ingressClassName: cloud-provider-kind`를 쓰고 host `shop.<namespace>.example`와 `api.<namespace>.example`의 rule을 각각 만듭니다.
- `shop.<namespace>.example`: `/`(Prefix)는 Service `shop-web` 포트 80, `/api`(Prefix)는 Service `shop-api` 포트 80입니다.
- `api.<namespace>.example`: `/`(Prefix)는 Service `shop-api` 포트 80입니다.
- 채점은 `Host` 헤더를 각 기준의 host로 바꿔 요청하므로 host 이름은 정확히 맞아야 합니다.

## 오답 피드백
- `all-to-web`: 모든 경로를 `shop-web`으로 보내면 `/`는 `web-ok`을 받아 통과하지만, `shop-web`도 `/api/`에 200으로 답하기 때문에 상태 코드만 보면 틀린 것을 알 수 없습니다. 채점은 본문을 보므로 `web-ok`을 받은 `api-path-route`와 `api-host-route`를 잃습니다 (4점: `web-ready`, `api-ready`, `root-route`, `ingress-shape`).
- `missing-api-host`: host `api.<namespace>.example`의 rule이 없으면 그 host의 요청을 받을 rule이 없어 `api-host-route`를 잃고, rule이 없다는 사실을 `ingress-shape`도 확인하므로 한 점을 더 잃습니다 (4점). host 없는 rule이나 `defaultBackend` 하나로 두 host를 받는 방법도 같은 이유로 `ingress-shape`를 통과하지 못합니다.
- `wrong-backend-port`: `shop-api`의 backend 포트를 Pod의 포트 8080으로 쓰면 Service에 8080 포트가 없어 `/api/` 요청이 `api-ok`에 도달하지 못합니다. `root-route`와 `ingress-shape`는 통과하지만 `api-path-route`와 `api-host-route`를 잃습니다 (4점).
- `wrong-class`: `ingressClassName: nginx`처럼 이 환경의 controller가 맡지 않는 class를 쓰면 Ingress에 주소가 할당되지 않아 세 요청이 모두 실패하고, `ingress-shape`도 class 값이 달라 잃습니다 (2점: `web-ready`, `api-ready`). `kubectl get ingress`의 `ADDRESS`가 비어 있고 `CLASS`가 `cloud-provider-kind`가 아니면 이 경우입니다.
- `alternative-default-class`: `ingressClassName`을 쓰지 않아도 admission이 기본 class `cloud-provider-kind`를 채우므로 저장된 Ingress는 정답과 같고 같은 controller가 받아 만점입니다 (6점). 채점은 저장된 Ingress의 `spec.ingressClassName`을 읽습니다.
- `alternative-port-name`: backend 포트를 이름 `http`로 지정하고 annotation이 더 붙어도 같은 Service 포트에 도달하므로 만점입니다 (6점).

## 채점
6개 기준을 각각 1점으로 채점합니다. `web-ready`와 `api-ready`는 제공된 Deployment의 replica가 Ready여야 하며(10초 안정 구간, 관찰 한도 90초) 각각 `root-route`, 그리고 `api-path-route`와 `api-host-route`가 평가되기 위한 전제입니다. `root-route`, `api-path-route`, `api-host-route`는 채점기가 Ingress `shop`의 주소로 실제 HTTP 요청을 보내며(60초 한도, 주소를 최대 15초 기다림), 각 기준이 자기 `Host` 헤더와 경로를 쓰고 상태 코드 200과 본문 `web-ok` 또는 `api-ok`을 모두 요구합니다. `ingress-shape`는 `spec.ingressClassName`이 `cloud-provider-kind`이고 두 host의 rule이 모두 있는지 확인합니다. 실행한 명령어 이력은 채점하지 않습니다.

## 공식 문서
- [Ingress](https://kubernetes.io/docs/concepts/services-networking/ingress/)
- [Ingress Controllers](https://kubernetes.io/docs/concepts/services-networking/ingress-controllers/)
- [Service](https://kubernetes.io/docs/concepts/services-networking/service/)
