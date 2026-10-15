#!/bin/sh
case "$1" in
  print) echo "order service v2" ;;
  serve) exec httpd -f -p 8080 -h /www ;;
  fail) echo "boom" >&2; exit 3 ;;
  *) echo "unknown mode" >&2; exit 2 ;;
esac
