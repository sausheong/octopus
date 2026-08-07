#!/usr/bin/env python3
"""T4b: extended latency — payload size, streaming, and concurrency.

The first T4 measured only a tiny non-streaming request. Octopus decodes and
re-encodes every request (T2), so overhead should grow with payload size; and
it forces stream:true upstream, so the streaming path is the one real clients
actually hit.
"""
import json
import statistics
import threading
import time
import urllib.request

MOCK = "http://127.0.0.1:9099/v1/messages"
OCTO = "http://127.0.0.1:8787/v1/messages"
PARA = "The quick brown fox jumps over the lazy dog. "


def payload(n_tokens_approx, stream=False, n_tools=0):
    filler = PARA * max(1, n_tokens_approx // 10)
    p = {
        "model": "mock/test-model", "max_tokens": 16,
        "system": [{"type": "text", "text": filler}],
        "messages": [{"role": "user", "content": "say ok"}],
    }
    if stream:
        p["stream"] = True
    if n_tools:
        p["tools"] = [{
            "name": f"tool_{i}", "description": "probe",
            "input_schema": {"type": "object", "properties": {
                f"p{j}": {"type": "string"} for j in range(8)}},
        } for i in range(n_tools)]
    return p


def post(url, p):
    body = json.dumps(p).encode()
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("anthropic-version", "2023-06-01")
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            r.read()
    except Exception:
        return None
    return (time.perf_counter() - t0) * 1000


def bench(url, p, n=120):
    for _ in range(8):
        post(url, p)
    s = [x for _ in range(n) if (x := post(url, p)) is not None]
    if not s:
        return None
    s.sort()
    return {"n": len(s), "p50": round(statistics.median(s), 2),
            "p95": round(s[int(len(s) * .95)], 2)}


print("=== A. overhead vs payload size (non-streaming in, ~bytes of JSON) ===")
print(f"  {'approx tokens':>14} {'bytes':>9} {'direct p50':>11} {'octopus p50':>12} {'overhead':>9}")
for approx in (100, 1_000, 10_000, 50_000, 150_000):
    p = payload(approx)
    size = len(json.dumps(p))
    d = bench(MOCK, p, 60)
    o = bench(OCTO, p, 60)
    if not d or not o:
        print(f"  {approx:>14,} {size:>9,}  FAILED")
        continue
    print(f"  {approx:>14,} {size:>9,} {d['p50']:>11.2f} {o['p50']:>12.2f} "
          f"{o['p50']-d['p50']:>+9.2f}")

print("\n=== B. streaming vs non-streaming (client-side request shape) ===")
for label, stream in (("non-streaming", False), ("streaming", True)):
    p = payload(1_000, stream=stream)
    d = bench(MOCK, p, 80)
    o = bench(OCTO, p, 80)
    if d and o:
        print(f"  {label:>14}: direct p50 {d['p50']:>6.2f}  octopus p50 {o['p50']:>6.2f}  "
              f"overhead {o['p50']-d['p50']:>+6.2f} ms")

print("\n=== C. tool-schema count (Octopus alphabetises every schema) ===")
for n_tools in (0, 5, 20, 60):
    p = payload(1_000, n_tools=n_tools)
    d = bench(MOCK, p, 60)
    o = bench(OCTO, p, 60)
    if d and o:
        print(f"  {n_tools:>3} tools: direct p50 {d['p50']:>6.2f}  octopus p50 {o['p50']:>6.2f}  "
              f"overhead {o['p50']-d['p50']:>+6.2f} ms")

print("\n=== D. concurrency (Octopus adds routing + session locking) ===")
for workers in (1, 4, 16):
    p = payload(1_000)
    for label, url in (("direct", MOCK), ("octopus", OCTO)):
        lat, lock = [], threading.Lock()

        def run():
            out = []
            for _ in range(20):
                v = post(url, p)
                if v is not None:
                    out.append(v)
            with lock:
                lat.extend(out)

        ts = [threading.Thread(target=run) for _ in range(workers)]
        t0 = time.perf_counter()
        [t.start() for t in ts]
        [t.join() for t in ts]
        wall = (time.perf_counter() - t0) * 1000
        if lat:
            lat.sort()
            print(f"  {workers:>2} workers {label:>8}: p50 {statistics.median(lat):>6.2f} ms  "
                  f"p95 {lat[int(len(lat)*.95)]:>6.2f} ms  wall {wall:>7.0f} ms  "
                  f"({len(lat)} reqs)")
