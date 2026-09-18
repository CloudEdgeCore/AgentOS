#!/usr/bin/env python3
"""AgentOS capacity-run driver (server side, Linux).

Runs TestControlPlanePipelineCapacityBaseline N times against a live PostgreSQL
and records every number needed to reproduce the published result.

Usage:
    run-capacity.py <tasks> <runs> <prefix> <database_url> [go_test_timeout]

Design notes (learned from the local 100K runs):
  * the worktree is pinned to a commit by the caller; this script never touches git
  * compilation is warmed once before run 1 so the compile step is not billed into
    the wall time of the first measured run
  * every 60 s a sampler appends swap / MemAvailable / PG-container RSS to
    <prefix>-samples.log, so "did swap get used?" is answerable from the raw log
  * the status file is rewritten after every state change, so a killed driver
    still leaves a truthful "where did it stop" record
"""
import datetime
import json
import os
import re
import subprocess
import sys
import threading
import time

REPO = os.path.expanduser("~/agentos")
OUT = os.path.expanduser("~/capacity-runs")
GO = "/usr/local/go/bin/go"
PG_CONTAINER = "agentos-cap-pg"
DURATION = re.compile(r"^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$")


def now():
    return datetime.datetime.now().strftime("%Y-%m-%dT%H:%M:%S")


def write_status(path, line):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(line + "\n")
    with open(path.replace(".status", "-driver.log"), "a", encoding="utf-8") as handle:
        handle.write(f"[{now()}] {line}\n")


def parse_duration(value):
    match = DURATION.match(value.strip())
    if not match:
        return None
    total = 0.0
    for part, scale in zip(match.groups(), (3600, 60, 1)):
        if part:
            total += float(part) * scale
    return total


def parse_log(text):
    record = {"pass": "--- PASS: TestControlPlanePipelineCapacityBaseline" in text}
    match = re.search(
        r"CAPACITY REPORT tasks=(\d+) wall=(\S+) throughput=([\d.]+) tasks/s", text
    )
    if match:
        record["tasks"] = int(match.group(1))
        record["wall"] = match.group(2)
        record["wall_s"] = parse_duration(match.group(2))
        record["throughput"] = float(match.group(3))
    for phase in ("enqueue", "admit", "schedule", "complete"):
        match = re.search(
            rf"CAPACITY REPORT {phase} wall=(\S+) throughput=([\d.]+) tasks/s", text
        )
        if match:
            record[phase] = {
                "wall": match.group(1),
                "wall_s": parse_duration(match.group(1)),
                "throughput": float(match.group(2)),
            }
    match = re.search(r"CAPACITY REPORT end-to-end p50=(\S+) p95=(\S+) p99=(\S+)", text)
    if match:
        record["end_to_end"] = {
            "p50": match.group(1),
            "p95": match.group(2),
            "p99": match.group(3),
            "p95_s": parse_duration(match.group(2)),
            "p99_s": parse_duration(match.group(3)),
        }
    match = re.search(r"^ok\s+\S+\s+([\d.]+)s", text, re.M)
    if match:
        record["go_test_s"] = float(match.group(1))
    return record


def meminfo():
    out = {}
    with open("/proc/meminfo", encoding="utf-8") as handle:
        for line in handle:
            if ":" in line:
                key, value = line.split(":", 1)
                out[key] = value.strip()
    return out


def diskstats():
    """(sectors_written, io_ticks_ms) for the root device, or None."""
    try:
        with open("/proc/diskstats", encoding="utf-8") as handle:
            for line in handle:
                parts = line.split()
                if len(parts) > 13 and parts[2] == "nvme0n1":
                    return int(parts[9]), int(parts[12])
    except Exception:
        pass
    return None


def sampler(stop, path, state):
    """Append host swap / memory, PG-container RSS and disk utilisation every 60 s.

    The disk numbers matter because this volume is gp3 at its 3000 IOPS
    baseline (measured: 3092 IOPS 4k random), so "was the disk the bottleneck?"
    has to be answerable from the raw log rather than assumed.
    """
    previous = diskstats()
    previous_at = time.time()
    while not stop.is_set():
        try:
            mem = meminfo()
            total = int(mem["SwapTotal"].split()[0])
            free = int(mem["SwapFree"].split()[0])
            rss = "-"
            try:
                result = subprocess.run(
                    ["docker", "stats", "--no-stream", "--format", "{{.MemUsage}}|{{.CPUPerc}}",
                     PG_CONTAINER],
                    capture_output=True, text=True, timeout=30,
                )
                rss = result.stdout.strip() or "-"
            except Exception:
                pass

            # Connection counts and database size: the Go test reports pipeline
            # throughput and phase latencies but nothing about the database's own
            # internal state, and baseline.md section 4 asks for the connection peak.
            # Kept O(1) — no count(*) on the task tables.
            db = "-"
            try:
                result = subprocess.run(
                    ["docker", "exec", PG_CONTAINER, "psql", "-tAc",
                     "select (select count(*) from pg_stat_activity)"
                     "||'/'||(select count(*) from pg_stat_activity where state = 'active')"
                     "||'/'||pg_size_pretty(pg_database_size('agentos'))",
                     "-U", "agentos", "-d", "agentos"],
                    capture_output=True, text=True, timeout=30,
                )
                db = result.stdout.strip() or "-"
            except Exception:
                pass

            disk = ""
            current = diskstats()
            current_at = time.time()
            if current and previous:
                span = max(current_at - previous_at, 1e-6)
                sectors = current[0] - previous[0]
                util = (current[1] - previous[1]) / span / 10.0  # ms busy per s -> %
                disk = f" disk_write_mibps={sectors * 512 / 1048576 / span:.2f} disk_util_pct={util:.1f}"
                state["peak_util"] = max(state.get("peak_util", 0.0), util)
                state["peak_write_mibps"] = max(state.get("peak_write_mibps", 0.0),
                                                sectors * 512 / 1048576 / span)
            previous, previous_at = current, current_at

            connections = 0
            try:
                connections = int(db.split("/", 1)[0])
            except Exception:
                pass
            state["peak_conn"] = max(state.get("peak_conn", 0), connections)

            with open(path, "a", encoding="utf-8") as handle:
                handle.write(
                    f"[{now()}] swap_used_mb={(total - free) // 1024} "
                    f"MemAvailable_mb={int(mem['MemAvailable'].split()[0]) // 1024} "
                    f"pg={rss} pg_conn_active_db={db}{disk}\n"
                )
        except Exception:
            pass
        stop.wait(60)


def main():
    if len(sys.argv) < 5:
        print(__doc__)
        return 2
    tasks, runs, prefix, database_url = (
        int(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4],
    )
    go_timeout = sys.argv[5] if len(sys.argv) > 5 else "240m"

    os.makedirs(OUT, exist_ok=True)
    status = os.path.join(OUT, f"{prefix}.status")
    summary = os.path.join(OUT, f"{prefix}-summary.json")
    samples = os.path.join(OUT, f"{prefix}-samples.log")

    env = dict(os.environ)
    env["AGENTOS_TEST_DATABASE_URL"] = database_url
    env["AGENTOS_CAPACITY_TASKS"] = str(tasks)
    env["PATH"] = "/usr/local/go/bin:" + env.get("PATH", "/usr/bin:/bin")
    env["HOME"] = os.path.expanduser("~")

    head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO,
                          capture_output=True, text=True).stdout.strip()
    dirty = subprocess.run(["git", "status", "--porcelain"], cwd=REPO,
                           capture_output=True, text=True).stdout.strip()
    goversion = subprocess.run([GO, "version"], capture_output=True, text=True).stdout.strip()

    write_status(status, f"START tasks={tasks} runs={runs} commit={head} "
                         f"dirty={'yes' if dirty else 'no'} {goversion}")

    stop = threading.Event()
    state = {}
    thread = threading.Thread(target=sampler, args=(stop, samples, state), daemon=True)
    thread.start()

    write_status(status, "WARM compile-only pass (not billed into run 1)")
    warm = subprocess.run(
        [GO, "test", "-tags=integration", "-count=1", "-run", "^$",
         "./internal/kernel/store/postgres/"],
        cwd=REPO, env=env, capture_output=True, text=True,
    )
    if warm.returncode != 0:
        write_status(status, f"ABORT warm-up failed exit={warm.returncode}")
        print(warm.stdout[-4000:])
        print(warm.stderr[-4000:])
        stop.set()
        return 1

    results = []
    for index in range(1, runs + 1):
        log_path = os.path.join(OUT, f"{prefix}-run{index}.log")
        write_status(status, f"RUNNING run={index}/{runs} phase=start started={now()}")
        started = time.time()
        with open(log_path, "w", encoding="utf-8") as handle:
            completed = subprocess.run(
                [GO, "test", "-tags=integration", "-count=1", "-timeout", go_timeout,
                 "-run", "^TestControlPlanePipelineCapacityBaseline$", "-v",
                 "./internal/kernel/store/postgres/"],
                cwd=REPO, env=env, stdout=handle, stderr=subprocess.STDOUT,
            )
        elapsed = time.time() - started
        with open(log_path, encoding="utf-8", errors="replace") as handle:
            text = handle.read()
        record = parse_log(text)
        record["run"] = index
        record["exit_code"] = completed.returncode
        record["elapsed_s"] = round(elapsed, 1)
        results.append(record)
        with open(summary, "w", encoding="utf-8") as handle:
            json.dump(results, handle, indent=2)
        if completed.returncode != 0:
            write_status(status, f"ABORT run={index}/{runs} exit={completed.returncode} "
                                 f"elapsed={elapsed / 60:.1f}min ended={now()}")
            stop.set()
            return 1
        if not record.get("pass"):
            write_status(status, f"ABORT run={index}/{runs} exit=0 but no PASS marker; "
                                 f"ended={now()}")
            stop.set()
            return 1
        write_status(status, f"RUNNING run={index}/{runs} done exit=0 "
                             f"elapsed={elapsed / 60:.1f}min throughput={record.get('throughput')} "
                             f"p95={record.get('end_to_end', {}).get('p95')}")

    stop.set()
    thread.join(timeout=5)

    mem = meminfo()
    swap_used_mb = (int(mem["SwapTotal"].split()[0]) - int(mem["SwapFree"].split()[0])) // 1024
    peaks = (f" peak_disk_util_pct={state.get('peak_util', 0):.1f} "
             f"peak_disk_write_mibps={state.get('peak_write_mibps', 0):.2f} "
             f"peak_pg_conn={state.get('peak_conn', 0)} "
             f"end_swap_used_mb={swap_used_mb}")

    throughputs = [r["throughput"] for r in results if r.get("throughput")]
    p95s = [r["end_to_end"]["p95_s"] for r in results if r.get("end_to_end", {}).get("p95_s")]
    if len(throughputs) == runs and len(p95s) == runs:
        tp_spread = (max(throughputs) / min(throughputs) - 1) * 100
        p95_spread = (max(p95s) / min(p95s) - 1) * 100
        write_status(
            status,
            f"SUCCESS tasks={tasks} commit={head} throughput_spread={tp_spread:.2f}% "
            f"p95_spread={p95_spread:.2f}% throughputs={throughputs} "
            f"p95_seconds={[round(v, 1) for v in p95s]} ended={now()}{peaks}",
        )
    else:
        write_status(status, f"DONE numbers_incomplete ended={now()}{peaks}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
