## 개념
Kubernetes에서 Pod는 ServiceAccount의 신원으로 API 서버를 호출합니다. ServiceAccount에는 아무 권한도 없으며, Role(네임스페이스 범위 규칙)이나 ClusterRole(클러스터 범위 규칙)을 RoleBinding/ClusterRoleBinding으로 묶어야 권한이 생깁니다. 규칙은 허용만 있고 거부 규칙이 없으므로, 필요한 verb·resource·resourceNames만 적는 것이 최소 권한(least privilege)입니다. Pod에 ServiceAccount를 지정하지 않으면 네임스페이스의 `default` ServiceAccount로 실행되고, 토큰은 `/var/run/secrets/kubernetes.io/serviceaccount/token`에 자동으로 마운트됩니다 (`automountServiceAccountToken: false`이면 마운트되지 않습니다).

중요한 점은 ClusterRole이더라도 RoleBinding으로 묶으면 그 네임스페이스에만 적용된다는 것입니다. 반대로 ClusterRoleBinding은 모든 네임스페이스(클러스터 전체)에 적용되므로 `kube-system` Pod 목록까지 열립니다. 또 학습자가 만든 클러스터 범위 객체(ClusterRole, ClusterRoleBinding)는 문제 reset 때 지워지지 않으므로 이 문제는 네임스페이스 범위 RBAC만 쓰라고 요구합니다.

## 진단 순서
1. `kubectl get deploy reporter -o jsonpath='{.spec.template.spec.serviceAccountName}'`으로 현재 Pod가 어떤 ServiceAccount로 실행되는지 확인합니다 (비어 있으면 `default`).
2. `kubectl create serviceaccount report-reader`로 ServiceAccount를 만들고 `kubectl create role`, `kubectl create rolebinding`으로 권한을 부여합니다.
3. `kubectl auth can-i get pods --as=system:serviceaccount:<namespace>:report-reader`처럼 허용해야 하는 동작과 허용하면 안 되는 동작(`delete pods`, `get secrets`, `create deployments`, `list pods -A`, `list pods -n kube-system`, `get configmap other`)을 모두 확인합니다.
4. `kubectl set serviceaccount deployment/reporter report-reader`(또는 manifest의 `spec.template.spec.serviceAccountName`)로 Deployment를 바꾸고 `kubectl rollout status deployment/reporter`로 새 Pod가 Ready가 되는지 봅니다.

## 해결
- ServiceAccount `report-reader`를 만듭니다.
- Role에 두 규칙을 둡니다: `pods`에 `get`, `list`, `watch`; `configmaps`에 `resourceNames: ["report-config"]`와 `get`. 이 Role을 RoleBinding으로 `report-reader`에 묶습니다. Role 두 개와 RoleBinding 두 개로 나누어도 같은 권한이므로 정답입니다.
- Deployment `reporter`의 Pod template에 `serviceAccountName: report-reader`를 지정합니다. 토큰 마운트를 끄지 않습니다.
- `default` ServiceAccount는 건드리지 않습니다.

## 오답 피드백
- Role을 `default` ServiceAccount에 묶으면 앱은 그대로 동작하는 것처럼 보이지만 `report-reader`가 없고 `default`가 권한을 얻으므로 이 문제의 요구와 반대입니다 (Ready 1점만 얻습니다).
- 내장 ClusterRole `edit`을 RoleBinding으로 묶으면 `can-read`와 `runs-as-sa`는 통과하지만, Pod 삭제·Secret 읽기·Deployment 생성·다른 ConfigMap 읽기가 허용되어 `least-privilege`에서 감점됩니다. 앱이 정상 동작해도 과한 권한은 점수를 잃습니다.
- ClusterRoleBinding으로 같은 규칙을 묶으면 모든 네임스페이스와 `kube-system`의 Pod를 list할 수 있어 `least-privilege`에서 감점됩니다. 이렇게 만든 클러스터 범위 객체는 reset으로 지워지지 않습니다.
- RBAC만 맞추고 Deployment가 계속 `default`로 실행되면 `runs-as-sa`에서 감점됩니다.

## 채점
5개 기준을 각각 1점으로 채점합니다. 권한 점수는 Role 이름·개수가 아니라 클러스터에 "이 ServiceAccount가 이 동작을 할 수 있는가"를 묻는 SubjectAccessReview 결과로 판정하므로, 같은 권한을 만드는 서로 다른 Role 구성은 모두 인정됩니다. `reporter-ready`는 replica 1개가 10초 안정 구간(관찰 한도 90초) 동안 Ready여야 합니다. `runs-as-sa`는 Pod의 serviceAccountName과 마운트된 토큰을 클러스터가 인증한 신원이 `report-reader`인지 확인하며 `reporter-ready`가 통과해야 평가합니다. 토큰은 증거와 결과에 저장되지 않습니다. 실행한 명령어 이력은 채점하지 않습니다.

## 공식 문서
- [Using RBAC Authorization](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [Configure Service Accounts for Pods](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/)
- [Checking API access](https://kubernetes.io/docs/reference/access-authn-authz/authorization/#checking-api-access)
