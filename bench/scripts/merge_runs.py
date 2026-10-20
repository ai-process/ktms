#!/usr/bin/env python3
"""Replaces some targets of a run with the same targets from a rerun, then rebuilds the report.

Usage: merge_runs.py OUT BASE TARGET=RERUN_DIR [...]
A rerun must include `direct` so its stand can be compared with the base stand; the report lists
every replaced target with its run, commit and direct-call baseline.
"""
import json
import shutil
import subprocess
import sys
from pathlib import Path


def main():
    out, base = Path(sys.argv[1]), Path(sys.argv[2])
    if out.exists():
        sys.exit(f"{out} exists")
    shutil.copytree(base, out)
    env = json.loads((out / "env.json").read_text())
    merged = []
    for arg in sys.argv[3:]:
        target, src = arg.split("=", 1)
        src = Path(src)
        src_env = json.loads((src / "env.json").read_text())
        for f in out.glob(f"{target}.*"):
            f.unlink()
        for f in src.glob(f"{target}.*"):
            shutil.copy2(f, out / f.name)
        image = src_env.get("sut", {}).get("images", {}).get(target)
        if image:
            env["sut"]["images"][target] = image
        direct = json.loads((src / "direct.latency.json").read_text())["metrics"]["http_req_duration"]["values"]
        merged.append({"target": target, "run_id": src_env.get("run_id"), "git_sha": src_env.get("git_sha"),
                       "direct_p50_ms": direct.get("med"), "direct_p99_ms": direct.get("p(99)")})
    env["merged"] = merged
    (out / "env.json").write_text(json.dumps(env))
    failed = out / "failed.jsonl"
    if failed.exists():
        keep = [l for l in failed.read_text().splitlines()
                if l.strip() and json.loads(l)["target"] not in {m["target"] for m in merged}]
        if keep:
            failed.write_text("\n".join(keep) + "\n")
        else:
            failed.unlink()
    for f in ("REPORT.md", "results.json"):
        (out / f).unlink(missing_ok=True)
    subprocess.run([sys.executable, str(Path(__file__).with_name("report.py")), str(out)], check=True,
                   stdout=subprocess.DEVNULL)


if __name__ == "__main__":
    main()
