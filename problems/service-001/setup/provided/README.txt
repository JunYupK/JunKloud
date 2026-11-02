Deployment catalog (2 replicas, container catalog on port 8080, container port named http) and Service catalog (port 80 named http) exist in ${namespace}. Deployment frontend requests http://catalog/ every 3 seconds and logs "frontend fetched <body>" or "frontend fetch failed". Service catalog does not reach the catalog Pods.
Repair Service catalog so that its port 80 (named http) routes to port 8080 of the Pods of Deployment catalog, and make it type NodePort with nodePort 30080.
Do not change the labels, the image or the command of the Pod template of Deployment catalog.
frontend must log "frontend fetched catalog-ok".
