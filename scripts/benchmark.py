#!/usr/bin/env python3
"""
Benchmark Octopus vs direct provider calls.

Usage:
    python3 scripts/benchmark.py [--runs N] [--concurrency C] [--output FILE]

Measures:
  - Time to first token (TTFT) for streaming requests
  - Total latency for buffered requests
  - Tokens/second for streaming requests
  - Routing overhead (router total - direct provider total)
  - Which model the router chose per prompt and how often
"""

import argparse
import json
import os
import statistics
import time
import threading
import urllib.request
from collections import Counter
from dataclasses import dataclass, field
from typing import Optional

# ── prompts ──────────────────────────────────────────────────────────────────

PROMPTS = [
    ("trivial",  "Reply with exactly one word: hello"),
    ("short",    "What is the capital of France? One sentence."),
    ("medium",   "Explain how TCP handshake works in 3 bullet points."),
    ("code",     "Write a Python function that returns the nth Fibonacci number."),
    ("long",     "Summarise the main causes of the First World War in 150 words."),
]

# ── endpoint configs ──────────────────────────────────────────────────────────

ROUTER_BASE    = "http://localhost:8787/v1"
ROUTER_API_KEY = "local"

# Direct provider — DeepSeek's OpenAI-compatible endpoint.
# Set DIRECT_API_KEY in the environment before benchmarking. Edit DIRECT_BASE /
# DIRECT_MODEL to point at any provider that speaks the OpenAI Chat Completions
# API.
DIRECT_BASE    = "https://api.deepseek.com/v1"
DIRECT_API_KEY = os.environ.get("DIRECT_API_KEY", "")
DIRECT_MODEL   = "deepseek-chat"   # DeepSeek's current V3 model alias

# ── types ─────────────────────────────────────────────────────────────────────

@dataclass
class Sample:
    prompt_label: str
    ttft_ms: Optional[float]        # time to first content token (streaming only)
    total_ms: float
    tokens_out: int
    tokens_per_sec: Optional[float]
    routed_model: Optional[str] = None  # model reported in the response body
    error: Optional[str] = None

@dataclass
class Result:
    label: str
    is_router: bool = False
    samples: list[Sample] = field(default_factory=list)

    def good(self):
        return [s for s in self.samples if s.error is None]

    def stat(self, values, fmt=".0f"):
        if not values:
            return "n/a"
        return (f"p50={statistics.median(values):{fmt}}  "
                f"p95={sorted(values)[int(len(values)*0.95)]:{fmt}}  "
                f"min={min(values):{fmt}}  max={max(values):{fmt}}")

# ── HTTP helpers ──────────────────────────────────────────────────────────────

def _headers(api_key: str) -> dict:
    return {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
    }

def call_buffered(base: str, api_key: str, model: str, prompt: str
                  ) -> tuple[float, int, Optional[str], Optional[str]]:
    """Returns (ms, output_tokens, routed_model, error)."""
    payload = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(
        f"{base}/chat/completions",
        data=payload,
        headers=_headers(api_key),
        method="POST",
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            body = json.loads(resp.read())
        ms = (time.perf_counter() - t0) * 1000
        tokens = body.get("usage", {}).get("completion_tokens", 0)
        routed = body.get("model")
        return ms, tokens, routed, None
    except Exception as e:
        ms = (time.perf_counter() - t0) * 1000
        return ms, 0, None, str(e)

def call_streaming(base: str, api_key: str, model: str, prompt: str
                   ) -> tuple[Optional[float], float, int, Optional[str], Optional[str]]:
    """Returns (ttft_ms, total_ms, tokens, routed_model, error)."""
    payload = json.dumps({
        "model": model,
        "stream": True,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()
    req = urllib.request.Request(
        f"{base}/chat/completions",
        data=payload,
        headers=_headers(api_key),
        method="POST",
    )
    t0 = time.perf_counter()
    ttft = None
    tokens = 0
    routed = None
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            for raw_line in resp:
                line = raw_line.decode("utf-8").strip()
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    break
                try:
                    chunk = json.loads(data)
                except json.JSONDecodeError:
                    continue
                if routed is None:
                    routed = chunk.get("model")
                delta = chunk.get("choices", [{}])[0].get("delta", {})
                content = delta.get("content", "")
                if content and ttft is None:
                    ttft = (time.perf_counter() - t0) * 1000
                if content:
                    tokens += max(1, len(content.split()))
        total_ms = (time.perf_counter() - t0) * 1000
        return ttft, total_ms, tokens, routed, None
    except Exception as e:
        total_ms = (time.perf_counter() - t0) * 1000
        return ttft, total_ms, tokens, routed, str(e)

# ── benchmark runners ─────────────────────────────────────────────────────────

def run_one(base: str, api_key: str, model: str,
            prompt_label: str, prompt: str, streaming: bool) -> Sample:
    if streaming:
        ttft, total, tokens, routed, err = call_streaming(base, api_key, model, prompt)
        tps = (tokens / (total / 1000)) if (total > 0 and tokens > 0 and not err) else None
        return Sample(prompt_label, ttft, total, tokens, tps, routed, err)
    else:
        total, tokens, routed, err = call_buffered(base, api_key, model, prompt)
        return Sample(prompt_label, None, total, tokens, None, routed, err)

def run_benchmark(label: str, base: str, api_key: str, model: str,
                  runs: int, concurrency: int, streaming: bool,
                  is_router: bool = False) -> Result:
    result = Result(label, is_router)
    lock = threading.Lock()
    tasks = [(pl, p) for pl, p in PROMPTS for _ in range(runs)]

    def worker(prompt_label, prompt):
        s = run_one(base, api_key, model, prompt_label, prompt, streaming)
        with lock:
            result.samples.append(s)
            routed_info = f"  → {s.routed_model}" if s.routed_model and is_router else ""
            status = (f"{'✓' if not s.error else '✗'} {label:30s} "
                      f"[{prompt_label:7s}] {s.total_ms:6.0f}ms{routed_info}")
            if s.ttft_ms:
                status += f"  ttft={s.ttft_ms:.0f}ms"
            if s.error:
                status += f"  ERR: {s.error[:60]}"
            print(status)

    sem = threading.Semaphore(concurrency)
    threads = []
    for pl, p in tasks:
        def go(pl=pl, p=p):
            with sem:
                worker(pl, p)
        t = threading.Thread(target=go)
        threads.append(t)
        t.start()
    for t in threads:
        t.join()
    return result

# ── reporting ─────────────────────────────────────────────────────────────────

def report(results: list[Result], streaming: bool, output: Optional[str]):
    lines = []
    lines.append("\n" + "═" * 76)
    lines.append(f"  BENCHMARK RESULTS  ({'streaming' if streaming else 'buffered'})")
    lines.append("═" * 76)

    for r in results:
        good = r.good()
        errors = len(r.samples) - len(good)
        lines.append(f"\n── {r.label} ({'streaming' if streaming else 'buffered'}) ──")
        lines.append(f"  Samples: {len(good)} ok, {errors} errors  "
                     f"({len(PROMPTS)} prompts × {len(good)//max(len(PROMPTS),1)} runs)")

        total_ms = [s.total_ms for s in good]
        lines.append(f"  Total latency (ms):   {r.stat(total_ms)}")

        if streaming:
            ttft_ms = [s.ttft_ms for s in good if s.ttft_ms is not None]
            tps = [s.tokens_per_sec for s in good if s.tokens_per_sec]
            lines.append(f"  TTFT (ms):            {r.stat(ttft_ms)}")
            lines.append(f"  Throughput (tok/s):   {r.stat(tps, '.1f')}")

        # Per-prompt breakdown with model chosen
        by_prompt = {}
        for s in good:
            by_prompt.setdefault(s.prompt_label, []).append(s)
        lines.append("  Per-prompt breakdown:")
        for pl, samples in sorted(by_prompt.items()):
            ms_vals = [s.total_ms for s in samples]
            model_counts = Counter(s.routed_model for s in samples if s.routed_model)
            model_str = "  ".join(f"{m} ×{n}" for m, n in model_counts.most_common())
            lines.append(f"    {pl:10s}: p50={statistics.median(ms_vals):.0f}ms  "
                         f"n={len(samples)}  {model_str}")

        # Overall model usage for router results
        if r.is_router:
            model_counts = Counter(s.routed_model for s in good if s.routed_model)
            if model_counts:
                lines.append("  Model selection (all prompts):")
                total = sum(model_counts.values())
                for m, n in model_counts.most_common():
                    pct = n / total * 100
                    lines.append(f"    {m}  ×{n}  ({pct:.0f}%)")

    # Overhead comparison
    if len(results) == 2:
        r0, r1 = results
        by_prompt0 = {}
        for s in r0.good():
            by_prompt0.setdefault(s.prompt_label, []).append(s.total_ms)
        by_prompt1 = {}
        for s in r1.good():
            by_prompt1.setdefault(s.prompt_label, []).append(s.total_ms)
        common = set(by_prompt0) & set(by_prompt1)
        if common:
            lines.append(f"\n── Routing overhead: {r1.label} vs {r0.label} ──")
            overheads = []
            for pl in sorted(common):
                overhead = statistics.median(by_prompt1[pl]) - statistics.median(by_prompt0[pl])
                overheads.append(overhead)
                sign = "+" if overhead >= 0 else ""
                lines.append(f"  {pl:10s}: {sign}{overhead:.0f}ms")
            avg = statistics.mean(overheads)
            sign = "+" if avg >= 0 else ""
            lines.append(f"  Average:    {sign}{avg:.0f}ms")

    lines.append("\n" + "═" * 76)
    text = "\n".join(lines)
    print(text)
    if output:
        with open(output, "w") as f:
            f.write(text + "\n")
        print(f"\nResults written to {output}")

# ── main ──────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="Benchmark Octopus vs direct provider")
    parser.add_argument("--runs",        type=int, default=3,    help="Runs per prompt (default: 3)")
    parser.add_argument("--concurrency", type=int, default=2,    help="Concurrent requests (default: 2)")
    parser.add_argument("--streaming",   action="store_true",    help="Use streaming mode (default: buffered)")
    parser.add_argument("--output",      type=str, default=None, help="Save results to file")
    parser.add_argument("--router-only", action="store_true",    help="Only benchmark the router")
    args = parser.parse_args()
    if not args.router_only and not DIRECT_API_KEY:
        parser.error("DIRECT_API_KEY must be set unless --router-only is used")

    print(f"Benchmark: runs={args.runs}  concurrency={args.concurrency}  "
          f"mode={'streaming' if args.streaming else 'buffered'}")
    print(f"Router:   {ROUTER_BASE}  (model=any → routed)")
    if not args.router_only:
        print(f"Direct:   {DIRECT_BASE}  (model={DIRECT_MODEL})")
    print()

    results = []

    if not args.router_only:
        print("── Running direct provider ──")
        results.append(run_benchmark(
            label=f"direct/{DIRECT_MODEL}",
            base=DIRECT_BASE,
            api_key=DIRECT_API_KEY,
            model=DIRECT_MODEL,
            runs=args.runs,
            concurrency=args.concurrency,
            streaming=args.streaming,
            is_router=False,
        ))
        print()

    print("── Running Octopus ──")
    results.append(run_benchmark(
        label="Octopus (routed)",
        base=ROUTER_BASE,
        api_key=ROUTER_API_KEY,
        model="any",
        runs=args.runs,
        concurrency=args.concurrency,
        streaming=args.streaming,
        is_router=True,
    ))

    report(results, args.streaming, args.output)

if __name__ == "__main__":
    main()
