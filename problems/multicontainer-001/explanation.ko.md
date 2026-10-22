init container가 공유 emptyDir에 message를 만들고 종료하면 sidecar가 이를 읽어 ack를 만듭니다. app은 ack를 읽어 로그로 보여 줍니다. 서로 다른 emptyDir를 mount하면 컨테이너가 모두 실행되어도 데이터가 전달되지 않습니다. 같은 mount 구조에서 잘못된 데이터도 실제 출력 검사로 실패합니다. 명령 문자열 자체는 채점하지 않습니다.
 app과 sidecar 양쪽에서 공유 파일의 최종 값을 읽어 실제 데이터도 검증하므로, 파일 없이 정답 로그만 출력하면 실패합니다. sizeLimit이 있는 emptyDir 및 다른 파일 도구도 허용합니다.
