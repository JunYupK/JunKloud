제공된 local chart를 parcel release로 설치하고 replicas와 message values를 덮어씁니다. chart 기본 image는 고정된 제공 alias입니다. -f 여러 파일 또는 동등한 Helm 명령을 사용할 수 있습니다.

실제 Helm release 저장 상태, chart 이름/버전, manifest 객체 소유권과 effective values를 검증합니다. kubectl apply로 같은 Deployment만 만들면 live 상태 점수는 얻을 수 있어도 release와 values 점수는 얻지 못합니다. 설치 후 live Deployment가 drift한 경우도 검출합니다. Ready는 현재 revision의 10초 안정성을 확인하며 실제 message 로그가 추가로 필요합니다.
