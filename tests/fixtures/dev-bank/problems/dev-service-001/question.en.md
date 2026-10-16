# Expose the web app with a Service

Weight: 6 points. Namespace: `dev-svc`.

Create a Service named `web` in namespace `dev-svc` that listens on port
`8080` and selects the pods labelled `app=web`.
