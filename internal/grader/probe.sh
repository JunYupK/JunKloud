#!/bin/sh
# JunKloud network probe. It runs inside a bounded, unprivileged pod built by
# network_probe.go and reports through the container termination message, so
# the grader never needs exec or logs. Input comes from the environment:
#
#   JK_PROTO     http | tcp | dns
#   JK_HOST      name or IP to dial
#   JK_PORT      destination port (not used by dns)
#   JK_HTTP_HOST Host header (http)
#   JK_PATH      request path (http)
#   JK_STATUS    expected status (http); an attempt that differs is retried
#   JK_MARKER    expected body marker (http), a fixed string, may be empty
#   JK_ATTEMPTS  maximum attempts
#   JK_TIMEOUT   per-attempt network timeout in seconds
#
# The message is "key=value" lines: v, dns, connected, class, status, marker,
# attempts, detail. The script always exits 0 once it has reported; a missing
# or unparsable message is a probe fault (never a learner fault).

N=${JK_ATTEMPTS:-3}
T=${JK_TIMEOUT:-3}
dns=skip
connected=0
class=
status=0
marker=0
detail=
attempts=0

dir=$(mktemp -d) || dir=/tmp
err=$dir/err
resp=$dir/resp
rcf=$dir/rc

now() { date +%s; }

finish() {
  printf 'v=1\ndns=%s\nconnected=%s\nclass=%s\nstatus=%s\nmarker=%s\nattempts=%s\ndetail=%s\n' \
    "$dns" "$connected" "$class" "$status" "$marker" "$attempts" "$detail" >/dev/termination-log
  exit 0
}

clean() { tr -cd 'a-zA-Z0-9 .:()_/,-' | head -c 160; }

# 1. Name resolution from this very pod (it carries the source identity).
case "$JK_HOST" in
  *:*) ;;                  # IPv6 literal
  *[!0-9.]*)
    dns=fail
    i=0
    while [ "$i" -lt "$N" ]; do
      i=$((i + 1))
      if timeout -s KILL $((T + 1)) nslookup "$JK_HOST" >"$dir/dns" 2>&1; then
        dns=ok
        break
      fi
      [ "$i" -lt "$N" ] && sleep 1
    done
    if [ "$dns" = fail ]; then
      class=dns
      detail=$(tail -n 2 "$dir/dns" | clean)
      finish
    fi
    ;;
esac
if [ "$JK_PROTO" = dns ]; then
  connected=1
  finish
fi

# 2. Connect, up to N attempts of at most T seconds each.
i=0
while [ "$i" -lt "$N" ]; do
  i=$((i + 1))
  attempts=$i
  : >"$err"
  : >"$resp"
  start=$(now)
  case "$JK_PROTO" in
    http)
      {
        printf 'GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: junkloud-probe\r\nConnection: close\r\n\r\n' "$JK_PATH" "$JK_HTTP_HOST" |
          timeout -s KILL $((T + 2)) nc -v -w "$T" "$JK_HOST" "$JK_PORT" 2>"$err"
        echo $? >"$rcf"
      } | head -c 65536 >"$resp"
      rc=$(cat "$rcf" 2>/dev/null || echo 1)
      ;;
    *)
      timeout -s KILL $((T + 2)) nc -v -w "$T" "$JK_HOST" "$JK_PORT" </dev/null >/dev/null 2>"$err"
      rc=$?
      ;;
  esac
  elapsed=$(($(now) - start))
  msg=$(clean <"$err")

  case "$msg" in
    *"bad address"*)
      dns=fail class=dns connected=0 detail=$msg
      finish
      ;;
  esac

  if [ "$JK_PROTO" = http ] && [ -s "$resp" ]; then
    # Something answered: connection established, judge the HTTP exchange.
    connected=1
    class=
    status=$(tr -d '\r' <"$resp" | sed -n '1s/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p')
    status=${status:-0}
    marker=0
    if [ -n "$JK_MARKER" ] && tr -d '\r' <"$resp" | sed '1,/^$/d' | grep -qF -- "$JK_MARKER"; then
      marker=1
    fi
    detail=
    if [ "$status" = "${JK_STATUS:-200}" ] && { [ -z "$JK_MARKER" ] || [ "$marker" = 1 ]; }; then
      finish
    fi
    class=http-mismatch
  elif [ "$rc" = 0 ]; then
    connected=1
    class=
    status=0
    if [ "$JK_PROTO" = tcp ]; then
      finish
    fi
    class=http-mismatch # connected but the server sent no HTTP answer
    detail="no HTTP response"
  else
    connected=0
    status=0
    marker=0
    case "$msg" in
      *"timed out"*) class=timeout ;;
      "")
        if [ "$elapsed" -ge "$T" ] || [ "$rc" = 137 ]; then class=timeout; else class=refused; fi
        ;;
      *refused*) class=refused ;;
      *) class=network-error ;;
    esac
    detail=$msg
  fi
  [ "$i" -lt "$N" ] && sleep 1
done
finish
