#!/bin/sh
set -eu

# Ensure common data/log paths exist and are writable by the app user.
# Named volumes often mount as root; fix ownership while still root.
mkdir -p /data/images /app/logs
chown codex2api:codex2api /data /data/images /app/logs 2>/dev/null || true

# Without TUNNEL_TOKEN, keep the original single-process path (PID 1 = app).
if [ -z "${TUNNEL_TOKEN:-}" ]; then
  exec su-exec codex2api:codex2api "$@"
fi

echo "TUNNEL_TOKEN set: starting codex2api + cloudflared" >&2

app_pid=""
tunnel_pid=""
exit_code=0

term_children() {
  if [ -n "${app_pid}" ] && kill -0 "${app_pid}" 2>/dev/null; then
    kill -TERM "${app_pid}" 2>/dev/null || true
  fi
  if [ -n "${tunnel_pid}" ] && kill -0 "${tunnel_pid}" 2>/dev/null; then
    kill -TERM "${tunnel_pid}" 2>/dev/null || true
  fi
}

wait_children() {
  if [ -n "${app_pid}" ]; then
    wait "${app_pid}" 2>/dev/null || true
  fi
  if [ -n "${tunnel_pid}" ]; then
    wait "${tunnel_pid}" 2>/dev/null || true
  fi
}

on_signal() {
  echo "received stop signal; shutting down" >&2
  exit_code=143
  term_children
  wait_children
  exit "${exit_code}"
}

trap on_signal TERM INT HUP

# cloudflared may write state under $HOME; keep it off root-owned paths.
export HOME=/tmp

su-exec codex2api:codex2api "$@" &
app_pid=$!

su-exec codex2api:codex2api cloudflared tunnel --no-autoupdate run --token "${TUNNEL_TOKEN}" &
tunnel_pid=$!

# Reap the first child that exits; stop the other.
while kill -0 "${app_pid}" 2>/dev/null && kill -0 "${tunnel_pid}" 2>/dev/null; do
  sleep 1
done

if ! kill -0 "${app_pid}" 2>/dev/null; then
  wait "${app_pid}" 2>/dev/null || exit_code=$?
  echo "codex2api exited with ${exit_code}; stopping cloudflared" >&2
else
  wait "${tunnel_pid}" 2>/dev/null || exit_code=$?
  echo "cloudflared exited with ${exit_code}; stopping codex2api" >&2
  # tunnel abnormal exit should not look like container success
  if [ "${exit_code}" -eq 0 ]; then
    exit_code=1
  fi
fi

term_children
wait_children
exit "${exit_code}"
