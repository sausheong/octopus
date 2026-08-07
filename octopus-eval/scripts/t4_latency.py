#!/usr/bin/env python3
"""T4: proxy latency overhead, direct-to-mock vs through-Octopus-to-mock."""
import json
import statistics
import sys
import time
import urllib.request

N = int(sys.argv[1]) if len(sys.argv) > 1 else 200
MOCK = "http://127.0.0.1:9099/v1/messages"
OCTO = "http://127.0.0.1:8787/v1/messages"

PAYLOAD = {
    "model": "mock/test-model",
    "max_tokens": 16,
    "messages": [{"role": "user", "content": "say ok"}],
}


def post(url, payload):
    body = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("anthropic-version", "2023-06-01")
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            r.read()
    except Exception as e:
        return None
    return (time.perf_counter() - t0) * 1000


def bench(name, url):
    for _ in range(10):  # warm up
        post(url, PAYLOAD)
    samples = [s for _ in range(N) if (s := post(url, PAYLOAD)) is not None]
    samples.sort()
    return {
        "arm": name, "n": len(samples),
        "p50_ms": round(statistics.median(samples), 2),
        "p95_ms": round(samples[int(len(samples) * 0.95)], 2),
        "mean_ms": round(statistics.mean(samples), 2),
    }


if __name__ == "__main__":
    d = bench("direct-to-mock", MOCK)
    o = bench("via-octopus", OCTO)
    print(json.dumps([d, o], indent=2))
    print(f"\noverhead p50: {round(o['p50_ms'] - d['p50_ms'], 2)} ms")
    print(f"overhead p95: {round(o['p95_ms'] - d['p95_ms'], 2)} ms")
