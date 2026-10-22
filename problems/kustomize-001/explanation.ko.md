base는 공용 원본이므로 수정하지 않고 overlay에서 replica와 ConfigMap 값을 변경합니다. replicas transformer, strategic merge 또는 JSON6902 patch 등 같은 객체를 만드는 방식을 허용합니다. 제공된 copy/만 참조하면 요구한 base를 사용한 것이 아닙니다.

채점은 base의 제공 bytes와 파일 목록을 비교하고 실제 overlay 그래프가 base에 도달하는지 검사합니다. render를 객체 단위로 비교하므로 YAML 순서나 표기 차이는 허용됩니다. 실제 Deployment/ConfigMap도 해당 render와 일치해야 합니다. raw apply만으로 정상 Pod를 만들더라도 overlay render 점수는 얻지 못합니다. Ready는 현재 revision에서 10초 안정성을 요구합니다.
