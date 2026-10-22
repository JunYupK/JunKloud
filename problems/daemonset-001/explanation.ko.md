DaemonSet은 대상 노드마다 Pod를 배치합니다. Linux nodeSelector와 필요한 toleration을 사용하고 Downward API로 실제 노드 이름을 읽습니다. Ready 판정은 DaemonSet 카운터뿐 아니라 현재 Pod의 안정성을 확인합니다. Deployment 대체와 잘못된 OS 선택은 오답이며 echo/printf 등 출력 구현은 자유입니다.
 NODE_NAME의 Downward API 연결도 검사하고, 각 Pod의 실제 spec.nodeName과 node= 로그 한 줄을 대조합니다. 비어 있거나 다른 노드 이름을 출력하면 정답이 아닙니다.
