## 개념
Pod Security Admission(PSA)은 쿠버네티스에 내장된 admission 컨트롤러로, 네임스페이스 레이블 `pod-security.kubernetes.io/enforce`가 가리키는 Pod Security Standard(`privileged`, `baseline`, `restricted`)를 Pod 생성 시점에 강제합니다. 이 문제의 네임스페이스는 `restricted`입니다. restricted는 모든 컨테이너에 `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `runAsNonRoot: true`(UID 0 금지), seccomp 프로파일 `RuntimeDefault`(또는 `Localhost`)를 요구합니다. PSA는 Pod만 검사하므로 Deployment 자체는 만들어지고, ReplicaSet이 Pod를 만들려 할 때 거부됩니다. 그래서 Deployment는 경고만 남기고 READY 0/1로 머물며, 진짜 원인은 ReplicaSet 이벤트(`FailedCreate`)에 나옵니다.

`securityContext`는 Pod 수준과 컨테이너 수준에 둘 수 있습니다. `runAsUser`, `runAsNonRoot`, `seccompProfile`은 양쪽에 있고 컨테이너 값이 Pod 값을 덮어씁니다. `allowPrivilegeEscalation`, `capabilities`, `readOnlyRootFilesystem`은 컨테이너 수준에만 있습니다. 읽기 전용 루트 파일시스템에서 앱이 쓰는 경로(`/www`)는 `emptyDir`처럼 쓰기 가능한 볼륨이어야 합니다. 이 문제의 앱이 1024 이상의 포트 8080을 쓰는 이유는 낮은 포트에 대한 가정을 피하기 위해서입니다. `capabilities.add: [NET_BIND_SERVICE]`를 넣어도 root가 아닌 프로세스에는 ambient capability가 없으므로 그 capability가 유효해지지 않고, 80번 같은 낮은 포트에 바인드할 수 있는지는 런타임이 Pod 네트워크 네임스페이스에 설정하는 `net.ipv4.ip_unprivileged_port_start` 값에 달려 있습니다 (런타임에 따라 0이어서 허용되기도 합니다).

## 진단 순서
1. `kubectl get deployment,replicaset,pods -n <namespace>`: Deployment는 0/1이고 Pod가 하나도 없습니다.
2. `kubectl describe replicaset -n <namespace>`의 Events에서 `FailedCreate` 메시지 `violates PodSecurity "restricted:latest": allowPrivilegeEscalation != false, unrestricted capabilities, runAsNonRoot != true, seccompProfile ...`을 읽습니다. 어떤 항목이 빠졌는지 그대로 나열해 줍니다.
3. `kubectl get namespace <namespace> --show-labels`로 어떤 정책이 강제되는지 확인합니다.
4. `kubectl edit deployment gateway`(또는 manifest 수정)로 컨테이너에 securityContext를 추가합니다. 새 ReplicaSet이 Pod를 만들고 Deployment가 Ready가 됩니다.

## 해결
컨테이너 `gateway`에 다음 securityContext를 추가합니다 (Pod 수준에 `runAsUser`, `runAsNonRoot`, `seccompProfile`을 두어도 동등합니다).

```yaml
securityContext:
  runAsUser: 10001
  runAsNonRoot: true
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
  seccompProfile:
    type: RuntimeDefault
```

네임스페이스 레이블을 `privileged`로 바꾸거나 지우면 Pod는 뜨지만 정책을 우회한 것이므로 `policy-enforced`를 잃습니다. image나 command를 바꾸는 것도 `workload-unchanged`를 잃습니다. 앱은 `/www`를 쓰기 가능한 `emptyDir`에 만들기 때문에 읽기 전용 루트 파일시스템에서도 동작합니다.

## 오답 피드백
- `policy-weakened`: 레이블을 `privileged`로 낮추고 워크로드는 그대로 두면 admission이 더는 막지 않아 Pod가 뜨지만 (`kubectl rollout restart`로 새 Pod를 받습니다), 정책을 바꿨으므로 `policy-enforced`를 잃고 워크로드의 securityContext도 선언되지 않아 `declared-context`를 잃습니다. `gateway-ready`와 `runtime-context`는 선행 기준이 실패했으므로 prerequisite not satisfied로 FAIL입니다. image와 command는 그대로라 `workload-unchanged`만 얻습니다 (1점).
- `policy-removed-and-hardened`: 레이블을 지우고 워크로드는 올바르게 고치면 모든 동작 기준은 통과하지만 정책이 더는 강제되지 않으므로 `policy-enforced`를 잃습니다 (4점). 정책을 지우면 이후의 어떤 Pod도 검사받지 않습니다. 이 항목이 "워크로드가 동작한다"와 "정책을 지켰다"를 구분합니다.
- `runs-as-root`: `runAsUser: 0`이면 restricted 정책이 Pod를 거부합니다(`runAsNonRoot != true` 또는 `runAsUser=0`). `declared-context`가 FAIL인 원인은 선언된 `runAsUser`가 0이라 요구값 10001과 다르기 때문입니다 (Pod가 거부된 것은 그 결과이지 FAIL의 원인이 아닙니다). 그래서 `gateway-ready`와 `runtime-context`도 선행 기준이 실패했으므로 FAIL입니다. 정책 레이블과 image·command는 그대로라 2점입니다. 이 기준들은 거부된 Pod 때문에 ERROR가 되지 않고, 선행 기준이 먼저 실패해 FAIL로 처리됩니다.
- `alternative-pod-level-context`: `runAsUser`, `runAsNonRoot`, `seccompProfile`을 Pod 수준에 두고 나머지를 컨테이너 수준에 두어도 유효한 securityContext가 같으므로 만점입니다 (5점).

## 채점
5개 기준을 각각 1점으로 채점합니다. `policy-enforced`는 Namespace 레이블 `pod-security.kubernetes.io/enforce`가 `restricted`인지 확인합니다. `workload-unchanged`는 컨테이너 `gateway`의 image와 command가 처음 값과 같은지 확인합니다. `declared-context`는 컨테이너 수준 값이 Pod 수준 값을 덮어쓰는 유효 값으로 `runAsUser` 10001, `runAsNonRoot`, `allowPrivilegeEscalation` false, `capabilities.drop` ALL, seccomp `RuntimeDefault`, `readOnlyRootFilesystem`을 확인합니다. `gateway-ready`는 `declared-context`가 통과해야 평가되며 Deployment의 replica 1개가 10초 안정 구간(관찰 한도 90초) 동안 Ready여야 합니다. `runtime-context`는 `gateway-ready`가 통과해야 평가되며, 실행 중인 Pod에서 `id -u`, `/proc/self/status`의 CapEff, `/proc/self/mountinfo`의 루트 마운트 옵션을 읽어 UID 10001, 유효 capability 없음, 읽기 전용 루트를 확인합니다.

순서가 이렇게 정해진 이유는 두 가지입니다. 첫째, 정책이 거부한 워크로드에는 Pod가 하나도 없습니다. Pod가 없는 `ready` 평가는 환경 문제(ERROR)로 처리되므로 선언 기준 `declared-context`가 먼저 실패해 `gateway-ready`와 `runtime-context`를 FAIL(prerequisite not satisfied)로 만듭니다. 둘째, 수정 직후의 새 Pod는 아직 Pending이어서 안을 읽을 수 없습니다. `ready`는 Pod가 Running이고 안정될 때까지 기다리므로, 그 뒤에 실행하는 `runtime-context`는 항상 Running Pod를 읽습니다.

알려진 경계 사례: `declared-context`는 통과하지만 다른 이유(예: restricted가 금지하는 `hostPath` 볼륨을 추가하거나 `hostNetwork`를 켜는 것)로 Pod가 여전히 거부되면 Pod가 하나도 없으므로 `gateway-ready`는 FAIL이 아니라 ERROR가 됩니다. 이 경우 ReplicaSet 이벤트(`FailedCreate`)에서 거부 사유를 읽고 고치세요. 실행한 명령어 이력은 채점하지 않습니다.

## 공식 문서
- [Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/)
- [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [Enforce Pod Security Standards with Namespace Labels](https://kubernetes.io/docs/tasks/configure-pod-container/enforce-standards-namespace-labels/)
- [Configure a Security Context for a Pod or Container](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/)
