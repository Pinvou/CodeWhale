#!/bin/sh
set -eu
# Liveness only. The disabled default user must never receive a PONG.
result=$(redis-cli --raw -s /run/redis/redis.sock PING 2>/dev/null) || exit 1
[ "$result" = 'NOAUTH Authentication required.' ]
