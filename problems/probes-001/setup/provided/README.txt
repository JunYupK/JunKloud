Deployment checkout and Service checkout already exist in ${namespace}. The application sleeps about 10 seconds before it starts serving on port 8080: / returns checkout-ok, /healthz and /ready return ok.
Use image docker.io/library/busybox:1.37.0 and imagePullPolicy: Never; do not change the image or command. Fix only the probes of container web.
