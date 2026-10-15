# Reviewed fixture for the image grading tests: one image, several behaviours
# selected by the CMD argument (print | serve | fail).
FROM docker.io/library/busybox:1.37.0
COPY entry.sh /app/entry.sh
COPY www /www
USER 10001
EXPOSE 8080
LABEL team="grading-fixture"
ENTRYPOINT ["/bin/sh", "/app/entry.sh"]
CMD ["print"]
