#!/usr/bin/env python3
"""Read-only Linux /proc sampler. No credentials, commands or environment output."""
import argparse
import json
import time
from pathlib import Path


def processes(proc=Path("/proc")):
    rows = {}
    for entry in proc.iterdir():
        if not entry.name.isdigit():
            continue
        try:
            fields = dict(line.split(":", 1) for line in (entry / "status").read_text().splitlines() if ":" in line)
            rows[int(entry.name)] = {
                "ppid": int(fields["PPid"]),
                "rss": int(fields.get("VmRSS", "0 kB").split()[0]) * 1024,
                "hwm": int(fields.get("VmHWM", "0 kB").split()[0]) * 1024,
            }
        except (OSError, ValueError, KeyError):
            continue  # Short-lived workers may exit during sampling.
    return rows


def descendants(rows, parent):
    found = {parent}
    while True:
        added = {pid for pid, row in rows.items() if row["ppid"] in found} - found
        if not added:
            return found - {parent}
        found.update(added)


def measure(pid, seconds, interval):
    peak_parent = peak_children = peak_total = peak_hwm = samples = 0
    seen = set()
    start = time.monotonic()
    while time.monotonic() - start < seconds:
        rows = processes()
        if pid not in rows:
            break
        children = descendants(rows, pid)
        seen.update(children)
        parent = rows[pid]["rss"]
        child = sum(rows[p]["rss"] for p in children)
        peak_parent = max(peak_parent, parent)
        peak_children = max(peak_children, child)
        peak_total = max(peak_total, parent + child)
        peak_hwm = max(peak_hwm, rows[pid]["hwm"])
        samples += 1
        time.sleep(interval)
    return dict(elapsedSeconds=round(time.monotonic()-start, 3), samples=samples,
                parentPeakSampledRSSBytes=peak_parent, parentLifetimeHWMBytes=peak_hwm,
                childrenPeakSampledRSSBytes=peak_children, combinedPeakSampledRSSBytes=peak_total,
                observedChildProcesses=len(seen), intervalSeconds=interval)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pid", required=True, type=int)
    parser.add_argument("--seconds", type=float, default=300)
    parser.add_argument("--interval", type=float, default=0.1)
    args = parser.parse_args()
    if args.pid <= 0 or args.seconds <= 0 or args.interval < 0.02:
        parser.error("positive PID/duration and interval >= 0.02 seconds required")
    if not Path("/proc/self/status").exists():
        parser.error("Linux /proc is required")
    print(json.dumps(measure(args.pid, args.seconds, args.interval), indent=2))
