#!/usr/bin/env python3
"""Builds REPORT.md and results.json from one run directory written by run_all.sh."""

import json
import statistics
import sys
from pathlib import Path

MIB = 1024 * 1024
REGISTRY = json.loads((Path(__file__).resolve().parent.parent / "gateways.json").read_text())
GATEWAYS = list(REGISTRY)
TARGETS = ["direct"] + GATEWAYS
LABELS = {"direct": "Direct (no gateway)", **{g: v["label"] for g, v in REGISTRY.items()}}
MACHINE = {"amd64": "x86_64", "arm64": "aarch64"}  # docker info arch -> uname -m


def load(out, name):
    p = out / name
    return json.loads(p.read_text()) if p.exists() else None


def trend(summary, metric):
    m = summary["metrics"].get(metric) if summary else None
    return m["values"] if m else None


def value(summary, metric, key):
    v = trend(summary, metric)
    return v.get(key) if v else None


def fmt(v, unit="", digits=2):
    return "–" if v is None else f"{v:.{digits}f}{unit}"


def table(head, rows):
    lines = ["| " + " | ".join(head) + " |", "|" + "---|" * len(head)]
    lines += ["| " + " | ".join(str(c) for c in r) + " |" for r in rows]
    return "\n".join(lines)


def latency(out):
    res, rows = {}, []
    base = load(out, "direct.latency.json")
    for t in TARGETS:
        s = load(out, f"{t}.latency.json")
        if not s:
            continue
        d = trend(s, "http_req_duration")
        r = {
            "rps": value(s, "http_reqs", "rate"),
            "failed": value(s, "http_req_failed", "rate"),
            **{k: d[k] for k in ("med", "p(90)", "p(95)", "p(99)", "max")},
        }
        if base and t != "direct":
            bd = trend(base, "http_req_duration")
            r["overhead_p50"] = d["med"] - bd["med"]
            r["overhead_p99"] = d["p(99)"] - bd["p(99)"]
        res[t] = r
        rows.append([LABELS[t], fmt(r["rps"], digits=0), fmt(r["med"], " ms"), fmt(r["p(95)"], " ms"),
                     fmt(r["p(99)"], " ms"), fmt(r.get("overhead_p50"), " ms"), fmt(r.get("overhead_p99"), " ms"),
                     fmt(r["failed"] * 100, " %")])
    md = table(["Target", "RPS", "p50", "p95", "p99", "+p50 vs direct", "+p99 vs direct", "Failed"], rows)
    return res, md


def streaming(out):
    res, rows = {}, []
    base = load(out, "direct.streaming.json")
    for t in TARGETS:
        s = load(out, f"{t}.streaming.json")
        if not s:
            continue
        ttft, total = trend(s, "ttft"), trend(s, "stream_total")
        if not ttft:
            continue
        r = {
            "ttft_p50": ttft["med"], "ttft_p95": ttft["p(95)"], "ttft_p99": ttft["p(99)"],
            "total_p50": total["med"], "streams": value(s, "http_reqs", "count"),
            "lost_chunks": value(s, "lost_chunks", "count") or 0,
            "failed": value(s, "stream_failed", "rate"),
        }
        if base and t != "direct" and trend(base, "ttft"):
            r["ttft_overhead_p50"] = ttft["med"] - trend(base, "ttft")["med"]
        res[t] = r
        rows.append([LABELS[t], fmt(r["ttft_p50"], " ms"), fmt(r["ttft_p95"], " ms"), fmt(r["ttft_p99"], " ms"),
                     fmt(r.get("ttft_overhead_p50"), " ms"), fmt(r["total_p50"], " ms"), int(r["lost_chunks"]),
                     fmt(None if r["failed"] is None else r["failed"] * 100, " %")])
    md = table(["Target", "TTFT p50", "TTFT p95", "TTFT p99", "+TTFT p50 vs direct", "Stream total p50",
                "Lost chunks", "Failed"], rows)
    return res, md


def saturation(out):
    res, rows, sustained = {}, [], []
    for t in TARGETS:
        s = load(out, f"{t}.saturation.json")
        if not s:
            continue
        p = s["params"]
        steps, best = [], None
        for rate in p["steps"]:
            count = value(s, f"http_reqs{{step:{rate}}}", "count") or 0
            ok = value(s, f"checks{{step:{rate}}}", "rate")
            d = trend(s, f"http_req_duration{{step:{rate}}}") or {}
            achieved = count / p["hold_s"]
            step = {"target_rps": rate, "achieved_rps": achieved, "ok": ok, "p50": d.get("med"), "p99": d.get("p(99)")}
            steps.append(step)
            # Sustained: the gateway keeps up with the offered rate with <1% errors.
            if ok is not None and ok >= 0.99 and achieved >= 0.95 * rate:
                best = rate
            rows.append([LABELS[t], rate, fmt(achieved, digits=0), fmt(step["p50"], " ms"), fmt(step["p99"], " ms"),
                         fmt(None if ok is None else (1 - ok) * 100, " %")])
        res[t] = {"steps": steps, "max_sustained_rps": best,
                  "dropped_iterations": value(s, "dropped_iterations", "count") or 0}
        sustained.append(f"- **{LABELS[t]}**: {best if best else 'below first step'} RPS sustained")
    md = table(["Target", "Offered RPS", "Achieved RPS", "p50", "p99", "Failed"], rows)
    return res, md + "\n\n" + "\n".join(sustained)


def resources(out, env):
    res, rows = {}, []
    for t in GATEWAYS:
        p = out / f"{t}.resources.jsonl"
        if not p.exists():
            continue
        ev = []
        for line in p.read_text().splitlines():
            try:
                ev.append(json.loads(line))
            except json.JSONDecodeError:
                print(f"skipping malformed line in {p.name}: {line!r}", file=sys.stderr)
        by = {e["phase"]: e for e in ev if e["phase"] != "soak_sample"}
        samples = [e["m"]["anon"] for e in ev if e["phase"] == "soak_sample"]
        soak = load(out, f"{t}.soak.json")
        reqs = value(soak, "http_reqs", "count")
        r = {"cold_start_ms": by.get("cold_start", {}).get("ms"),
             "arch": by.get("arch", {}).get("arch"),
             "image_bytes": env.get("sut", {}).get("images", {}).get(t, {}).get("bytes")}
        if "idle" in by:
            r["idle_anon"] = by["idle"]["m"]["anon"]
        if samples:
            r["soak_anon_avg"] = statistics.mean(samples)
            q = max(1, len(samples) // 4)
            # Drift between the first and last quarter of the soak; a leak grows steadily.
            r["soak_drift"] = statistics.mean(samples[-q:]) - statistics.mean(samples[:q])
        if "soak_start" in by and "soak_end" in by and reqs:
            cpu = by["soak_end"]["m"]["cpu_usec"] - by["soak_start"]["m"]["cpu_usec"]
            r["cpu_ms_per_1k"] = cpu / 1000 / reqs * 1000
        if "upstream_start" in by and "upstream_end" in by and reqs:
            r["upstream_hits"] = by["upstream_end"]["u"]["requests"] - by["upstream_start"]["u"]["requests"]
            r["soak_requests"] = reqs
        if "cooled" in by:
            r["cooled_anon"] = by["cooled"]["m"]["anon"]
        last = by.get("after_saturation") or by.get("cooled")
        if last:
            r["peak"] = last["m"]["peak"] or None
        res[t] = r
        mib = lambda k: fmt(r[k] / MIB, " MiB", 1) if r.get(k) is not None else "–"
        rows.append([LABELS[t], r["arch"] or "–", fmt(r["image_bytes"] / MIB if r.get("image_bytes") else None, " MiB", 0),
                     fmt(r["cold_start_ms"], " ms", 0), mib("idle_anon"), mib("soak_anon_avg"), mib("peak"),
                     mib("cooled_anon"), mib("soak_drift"), fmt(r.get("cpu_ms_per_1k"), " ms", 1)])
    md = table(["Gateway", "Arch", "Image", "Cold start", "Idle RSS", "RSS under soak", "Peak (cgroup)",
                "RSS after cooldown", "Soak drift", "CPU per 1k req"], rows)
    return res, md


def gateways_table(env, out):
    images = env.get("sut", {}).get("images", {})
    rows = [[LABELS[g], f"`{images.get(g, {}).get('image', '–')}`", "yes" if REGISTRY[g].get("auth", True) else "no",
             REGISTRY[g].get("note", "")] for g in GATEWAYS if (out / f"{g}.latency.json").exists()]
    return table(["Gateway", "Image", "Client auth", "Notes"], rows)


def warnings(env, rs, aborted, failed):
    host = MACHINE.get(env.get("sut", {}).get("arch"), env.get("sut", {}).get("arch"))
    out = [f"- ⛔ {LABELS.get(f['target'], f['target'])} failed (exit {f['exit']}) and was skipped; "
           "its rows are partial or missing." for f in failed]
    if aborted:
        out.append(f"- ⛔ Run aborted during `{aborted['target']}` (exit {aborted['exit']}); "
                   "tables below hold only what was measured before that.")
    for t, r in rs.items():
        if r.get("arch") and host and r["arch"] != host:
            out.append(f"- ⚠️ {LABELS[t]} ran as `{r['arch']}` on a `{host}` host (emulated): its numbers are "
                       "not comparable. Use cloud mode on x86_64.")
        if "upstream_hits" in r and r["upstream_hits"] < r["soak_requests"]:
            out.append(f"- ⚠️ {LABELS[t]}: {r['upstream_hits']} upstream hits for {r['soak_requests']} soak "
                       "requests; some responses did not come from the mock.")
    for m in env.get("merged", []):
        out.append(f"- ℹ️ {LABELS.get(m['target'], m['target'])} comes from rerun `{m['run_id']}` (commit `{m['git_sha']}`) "
                   f"on a separate stand of the same shape; its direct baseline there was p50 {fmt(m['direct_p50_ms'], ' ms')}, "
                   f"p99 {fmt(m['direct_p99_ms'], ' ms')}.")
    if env.get("mode") == "local":
        out.append("- Local mode: k6 and the gateways share one machine. Use it for regressions, "
                   "not for published numbers.")
    return "\n".join(out)


def main():
    out = Path(sys.argv[1])
    env = load(out, "env.json") or {}
    lat, lat_md = latency(out)
    st, st_md = streaming(out)
    sat, sat_md = saturation(out)
    sat_p = next((s["params"] for t in TARGETS if (s := load(out, f"{t}.saturation.json"))), {})
    rs, rs_md = resources(out, env)
    sut = env.get("sut", {})
    images = ", ".join(f"`{v['image']}`" for v in sut.get("images", {}).values())
    lat_p = (load(out, "direct.latency.json") or {}).get("params", {})
    st_p = (load(out, "direct.streaming.json") or {}).get("params", {})
    soak_p = next((s["params"] for g in GATEWAYS if (s := load(out, f"{g}.soak.json"))), {})

    failed_path = out / "failed.jsonl"
    failed = [json.loads(l) for l in failed_path.read_text().splitlines() if l.strip()] if failed_path.exists() else []
    warn = warnings(env, rs, load(out, "aborted.json"), failed)
    note = f" ({env['note']})" if env.get("note") else ""

    report = f"""# Gateway benchmark report

Run `{env.get('run_id')}` · profile `{env.get('profile')}` · mode `{env.get('mode')}`{note} · commit `{env.get('git_sha')}` · {env.get('started_at')}

SUT: {sut.get('cpus')} CPUs ({sut.get('arch')}), {fmt((sut.get('mem_bytes') or 0) / 2**30, ' GiB', 1)} RAM, kernel `{sut.get('kernel')}`, Docker {sut.get('docker')}.
Images: {images}.

{warn}

## 0. Gateways

Every gateway points at the same mock with retries and caching off, and access logs and telemetry off
wherever the gateway allows it (see notes).

{gateways_table(env, out)}

## 1. Latency overhead

Non-streaming, {lat_p.get('rate')} RPS for {lat_p.get('duration')}, upstream latency {lat_p.get('latency_ms')} ms.
"+ vs direct" is the gateway's added latency over calling the mock directly.

{lat_md}

## 2. Streaming and TTFT

{st_p.get('vus')} concurrent streams of {st_p.get('chunks')} chunks, {st_p.get('chunk_delay_ms')} ms apart, for {st_p.get('duration')}.
TTFT is time to first byte; the mock sends headers with the first token. Stream total near
{(st_p.get('chunks', 1) - 1) * st_p.get('chunk_delay_ms', 0)} ms means no buffering.

{st_md}

## 3. Saturation

Stepped arrival rate, {sat_p.get('latency_ms')} ms upstream latency, {sat_p.get('hold_s')} s per step. A step is sustained when achieved RPS is at least 95% of
offered and fewer than 1% of requests fail.

{sat_md}

## 4. Resources

RSS is cgroup `anon` memory of the gateway container (dependencies such as Postgres and Redis are
excluded). Cold start is `docker start` to the first successful chat completion through the gateway. Soak: {soak_p.get('rate')} RPS for {soak_p.get('duration')}. Drift compares the first
and last quarter of soak samples. CPU per 1k requests is cgroup CPU time over the soak.

{rs_md}
"""
    (out / "REPORT.md").write_text(report)
    (out / "results.json").write_text(json.dumps(
        {"env": env, "latency": lat, "streaming": st, "saturation": sat, "resources": rs}, indent=2))
    print(report)


if __name__ == "__main__":
    main()
