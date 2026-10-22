기존 Deployment의 현재 Pod template을 안정 버전으로 수정합니다. image는 제공된 native alias를 사용하고 RELEASE를 stable-v1로 지정합니다. replicas 2, RollingUpdate의 maxSurge 1/maxUnavailable 0을 확인하세요.

Ready 검사는 observedGeneration과 updated/ready/available 수뿐 아니라 현재 revision의 실제 Pod가 10초 동안 재시작 없이 안정적인지 확인합니다. readinessProbe가 계속 실패하는 상태나 옛 revision만 정상인 상태는 완료가 아닙니다. 로그도 모든 대상 Pod에서 version=stable-v1 한 줄이 확인되어야 합니다. 과거 rollout undo 실행 이력을 추측하거나 특정 revision 번호를 요구하지 않습니다. echo와 printf 등 동등한 출력은 허용됩니다.
