Deployment shop, Service shop, ConfigMap shop-config (key currency) and Secret shop-secret (key api-token) already exist in ${namespace}. Container app reads the environment variables CURRENCY and API_TOKEN, but its Pod cannot start.
Take CURRENCY from ConfigMap shop-config key currency and API_TOKEN from Secret shop-secret key api-token, both with valueFrom references. Do not change the value of currency in shop-config.
Use image docker.io/library/busybox:1.37.0 and imagePullPolicy: Never.
