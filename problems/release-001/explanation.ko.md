stable과 canary를 각각 1개 replica로 운영하고 app/track label 및 RELEASE 값을 지정합니다. Service의 공통 selector가 두 Deployment의 Ready Pod를 모두 선택해야 합니다. 기존 selector의 track: stable을 제거하지 않으면 canary가 제외됩니다.

채점은 임의 HTTP 요청 비율을 추정하지 않습니다. Service UID부터 EndpointSlice, Pod, ReplicaSet, Deployment까지 실제 소유 관계를 검증하고 각 backend에 직접 연결해 STABLE/CANARY 본문을 확인합니다. Pod의 설정만 맞아도 실제 응답이 잘못되면 마지막 1점을 얻지 못합니다. 동등한 공통 label selector를 허용하며 numeric targetPort에는 containerPort 선언이 필수가 아닙니다.
