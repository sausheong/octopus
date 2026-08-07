#!/usr/bin/env python3
"""T4c: proxy overhead as a function of payload size.

The original T4 measured a 591-byte request and reported +0.27ms, which is not
representative — Octopus decodes and re-encodes every request (T2), so overhead
grows with payload.

Requires mock_upstream.py on :9099 and octotest on :8787 configured with a
LARGE max_context (see configs/config-mock-big.yaml) — otherwise big requests
are rejected by the eligibility filter rather than measured.
"""
import json
import statistics
import time
import urllib.request

MOCK = "http://127.0.0.1:9099/v1/messages"
OCTO = "http://127.0.0.1:8787/v1/messages"
PARA = "The quick brown fox jumps over the lazy dog. "
SIZES = (100, 1_000, 10_000, 50_000, 150_000, 400_000)


def payload(approx_tokens):
    return {
        "model": "mock/test-model", "max_tokens": 16,
        "system": [{"type": "text", "text": PARA * max(1, approx_tokens // 10)}],
        "messages": [{"role": "user", "content": "say ok"}],
    }


def post(url, p):
    req = urllib.request.Request(url, data=json.dumps(p).encode(), method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("anthropic-version", "2023-06-01")
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            r.read()
    except Exception:
        return None
    return (time.perf_counter() - t0) * 1000


def bench(url, p, n=40):
    for _ in range(5):
        post(url, p)
    s = [v for _ in range(n) if (v := post(url, p)) is not None]
    if not s:
        return None
    s.sort()
    return statistics.median(s)


print(f"  {'approx tok':>11} {'bytes':>10} {'direct':>9} {'octopus':>9} {'overhead':>10} {'ratio':>7}")
for a in SIZES:
    p = payload(a)
    size = len(json.dumps(p))
    d, o = bench(MOCK, p), bench(OCTO, p)
    if d is None or o is None:
        print(f"  {a:>11,} {size:>10,}  FAILED (check max_context in the octotest config)")
        continue
    print(f"  {a:>11,} {size:>10,} {d:>9.2f} {o:>9.2f} {o-d:>+10.2f} {o/d:>6.1f}x")

print("\n  Terra's measured context: median 165k, mean 257k -> expect ~12-20 ms overhead.")
