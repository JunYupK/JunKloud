## 개념
Kubernetes는 오래된 API 버전을 deprecation 기간 뒤에 제거합니다. v1.37 API 서버는 `extensions/v1beta1` Deployment(v1.16에서 제거)와 `autoscaling/v2beta1` HorizontalPodAutoscaler(v1.25에서 제거)를 제공하지 않으므로, 그대로 `kubectl apply`하면 "no matches for kind" 오류가 납니다. `apiVersion` 한 줄만 바꾸면 끝나는 경우는 드뭅니다.

## 진단 순서
1. `kubectl apply -n <namespace> -f legacy/app.yaml`을 실행하거나 `kubectl api-versions`, `kubectl api-resources`로 현재 제공되는 group/version을 확인합니다.
2. `kubectl explain deployment.spec`, `kubectl explain hpa.spec.metrics.resource.target`으로 새 버전의 필수 필드를 확인합니다.
3. 파일을 수정하고 실제로 적용한 뒤 `kubectl get deploy,hpa,svc`와 `kubectl get endpointslices -l kubernetes.io/service-name=inventory`로 확인합니다.

## 해결
- Deployment는 `apps/v1`로 바꾸고 `spec.selector.matchLabels`를 추가합니다. selector는 Pod template label과 일치해야 하며, Service selector(`app: inventory`, `tier: api`)가 선택하는 label을 그대로 유지해야 endpoint가 생깁니다. `matchExpressions` selector도 동등합니다.
- HPA는 `autoscaling/v2`로 바꾸고 `targetAverageUtilization: 70`을 `target: {type: Utilization, averageUtilization: 70}`으로 바꿉니다. `scaleTargetRef.apiVersion`도 `apps/v1`, `kind`는 `Deployment`로 맞춰야 합니다. 옛 `extensions/v1beta1`을 그대로 두면 API 서버가 객체 자체는 받아들이지만 HPA가 대상 Deployment를 찾지 못해(`FailedGetScale`) 동작하지 않으므로 file과 live 모두 감점됩니다. `behavior` 같은 추가 설정이 있어도 요구 값이 같으면 허용됩니다. 채점은 첫 번째 `type: Resource` metric을 보므로 CPU metric(`name: cpu`)을 첫 Resource metric으로 두어야 하고, 다른 metric은 그 뒤에 추가할 수 있습니다.
- 수정한 파일을 `kubectl apply -n <namespace> -f legacy/app.yaml`로 적용합니다.

## 채점
파일 내용(2점)과 실제 클러스터 상태(3점)를 따로 채점합니다. HPA는 `autoscaling/v2`, 대상 `apps/v1` Deployment `inventory`, 2~4 범위, CPU `Utilization` 70%를 확인합니다. 파일만 고치고 적용하지 않으면 live 점수가 없고, 파일과 다른 값을 적용하면 file 점수와 live 점수가 어긋납니다. selector에서 `tier` label을 빠뜨리면 Deployment는 Ready여도 Service endpoint가 비어 마지막 1점을 얻지 못합니다. 사용한 명령어 이력은 채점하지 않으며 v1.37이 실제로 받아들이는 객체만 확인합니다.

## 공식 문서
- [Deprecated API Migration Guide](https://kubernetes.io/docs/reference/using-api/deprecation-guide/)
- [API Overview: API versioning](https://kubernetes.io/docs/reference/using-api/#api-versioning)
- [Deployments](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)
- [Horizontal Pod Autoscaling](https://kubernetes.io/docs/tasks/run-application/horizontal-pod-autoscale/)
