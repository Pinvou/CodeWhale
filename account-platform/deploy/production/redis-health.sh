#!/bin/sh
set -eu
# Liveness only: unauthenticated local PING must not obtain a PONG.
result=$(redis-cli --raw -s /run/redis/redis.sock PING 2>/dev/null) || exit 1
[ "$result" = 'NOAUTH Authentication required.' ]
