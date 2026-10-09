#!/bin/bash
set -u

redis-server --bind 127.0.0.1 --port 6379 --daemonize no \
    --dir /var/lib/redis --appendonly yes &
redis_pid=$!
ci_pid=

stop_services() {
    trap - TERM INT
    if [ -n "$ci_pid" ]; then
        kill -TERM "$ci_pid" 2>/dev/null || true
        wait "$ci_pid" 2>/dev/null || true
    fi
    kill -TERM "$redis_pid" 2>/dev/null || true
    wait "$redis_pid" 2>/dev/null || true
}

trap 'stop_services; exit 0' TERM INT

while [ "$(redis-cli -h 127.0.0.1 -p 6379 ping 2>/dev/null)" != PONG ]; do
    if ! kill -0 "$redis_pid" 2>/dev/null; then
        wait "$redis_pid"
        exit "$?"
    fi
    sleep 0.1
done

/usr/local/bin/ci "$@" &
ci_pid=$!

# If either service exits, stop the other; keep Redis alive until the bot stops.
wait -n "$redis_pid" "$ci_pid"
exit_status=$?
stop_services
exit "$exit_status"
