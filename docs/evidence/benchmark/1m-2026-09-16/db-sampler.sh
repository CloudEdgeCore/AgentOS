#!/usr/bin/env bash
# Side sampler for the capacity runs: records PostgreSQL connection counts and
# database growth once a minute. The Go test reports pipeline throughput and
# phase latencies but nothing about the database's own internal state, and
# baseline.md section 4 asks for "DB Connections (peak)" — this fills that gap.
#
# Deliberately avoids `select count(*) from tasks` (a full scan on a 3000-IOPS
# gp3 volume); pg_class.reltuples is an O(1) estimate.
set -uo pipefail
PREFIX="${1:?usage: db-sampler.sh <prefix>}"
OUT="$HOME/capacity-runs/${PREFIX}-dbsamples.log"
NAME=agentos-cap-pg

while true; do
  line=$(docker exec "$NAME" psql -tAc "select
      'conn=' || (select count(*) from pg_stat_activity)
   || ' active=' || (select count(*) from pg_stat_activity where state = 'active')
   || ' idle_in_tx=' || (select count(*) from pg_stat_activity where state like 'idle in transaction%')
   || ' db=' || pg_size_pretty(pg_database_size('agentos'))
   || ' tasks_est=' || coalesce((select reltuples::bigint from pg_class where relname='tasks'), -1)
   || ' wal=' || pg_size_pretty(coalesce((select sum(size) from pg_ls_waldir()), 0))" \
      -U agentos -d agentos 2>/dev/null)
  echo "[$(date -Is)] ${line:-unavailable}" >> "$OUT"
  sleep 60
done
