legacy/app.yaml was written for an older Kubernetes release and is rejected by the current API server.
Update legacy/app.yaml in place so every object uses a served API, then apply it to ${namespace}.
Service inventory already exists and selects Pods labeled app=inventory and tier=api.
