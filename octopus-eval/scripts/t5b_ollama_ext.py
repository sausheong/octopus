#!/usr/bin/env python3
"""T5b: extended local-model tests through Octopus -> Ollama.

Beyond "does it respond": throughput, real usable context, tool calling, and
concurrency — the things that decide whether local inference is usable for
agent work rather than a demo.
"""
import json
import statistics
import threading
import time
import urllib.request

OCTO = "http://127.0.0.1:8788/v1/messages"
PARA = "The quick brown fox jumps over the lazy dog. "


def post(payload, timeout=600):
    req = urllib.request.Request(OCTO, data=json.dumps(payload).encode(), method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("anthropic-version", "2023-06-01")
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            d = json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return {"error": e.read().decode()[:180], "s": time.time() - t0}
    except Exception as e:
        return {"error": str(e)[:180], "s": time.time() - t0}
    u = d.get("usage", {}) or {}
    txt = "".join(b.get("text", "") for b in d.get("content", []) if b.get("type") == "text")
    tools = [b for b in d.get("content", []) if b.get("type") == "tool_use"]
    return {"s": time.time() - t0, "in": u.get("input_tokens"),
            "out": u.get("output_tokens"), "text": txt.strip()[:70],
            "tool_calls": len(tools),
            "tool_name": tools[0].get("name") if tools else None}


def base(user, max_tokens=64, ctx_tokens=0, tools=None):
    p = {"model": "ollama/qwen2.5:3b", "max_tokens": max_tokens,
         "messages": [{"role": "user", "content": user}]}
    if ctx_tokens:
        p["system"] = [{"type": "text", "text": PARA * max(1, ctx_tokens // 10)}]
    if tools:
        p["tools"] = tools
    return p


print("=== A. generation throughput (tokens/sec) ===")
print(f"  {'max_tokens':>10} {'out':>5} {'seconds':>9} {'tok/s':>8}")
for mt in (16, 64, 256, 512):
    r = post(base("Write a detailed paragraph about distributed systems.", max_tokens=mt))
    if "error" in r:
        print(f"  {mt:>10} ERROR {r['error'][:60]}")
        continue
    tps = (r["out"] or 0) / r["s"] if r["s"] else 0
    print(f"  {mt:>10} {r['out']:>5} {r['s']:>9.2f} {tps:>8.1f}")

print("\n=== B. real usable context (declared cap 32,768) ===")
print(f"  {'ctx tokens':>11} {'result':>10} {'seconds':>9}  note")
for ctx in (1_000, 4_000, 8_000, 16_000, 28_000, 31_000):
    r = post(base("Reply with one word: ok", max_tokens=16, ctx_tokens=ctx), timeout=900)
    if "error" in r:
        note = r["error"][:70].replace("\n", " ")
        print(f"  {ctx:>11,} {'ERROR':>10} {r['s']:>9.1f}  {note}")
    else:
        print(f"  {ctx:>11,} {'ok':>10} {r['s']:>9.1f}  in={r['in']} out={r['out']} "
              f"reply={r['text'][:24]!r}")

print("\n=== C. tool calling through the Anthropic shape ===")
tools = [{"name": "get_weather", "description": "Get weather for a city",
          "input_schema": {"type": "object",
                           "properties": {"city": {"type": "string"}},
                           "required": ["city"]}}]
r = post(base("What is the weather in Singapore? Use the tool.", max_tokens=128, tools=tools))
if "error" in r:
    print(f"  ERROR: {r['error'][:150]}")
else:
    print(f"  tool_calls={r['tool_calls']} name={r['tool_name']} "
          f"text={r['text'][:60]!r} ({r['s']:.1f}s)")

print("\n=== D. concurrency (local GPU/CPU is a single shared resource) ===")
for workers in (1, 2, 4):
    lat, lock = [], threading.Lock()

    def run():
        out = []
        for _ in range(2):
            r = post(base("Count to five.", max_tokens=48))
            if "error" not in r:
                out.append(r["s"])
        with lock:
            lat.extend(out)

    ts = [threading.Thread(target=run) for _ in range(workers)]
    t0 = time.time()
    [t.start() for t in ts]
    [t.join() for t in ts]
    wall = time.time() - t0
    if lat:
        print(f"  {workers:>2} workers: median {statistics.median(lat):>6.2f}s  "
              f"wall {wall:>6.2f}s  ({len(lat)} reqs)")
