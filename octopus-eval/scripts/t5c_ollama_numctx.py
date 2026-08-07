#!/usr/bin/env python3
"""Measure useful bounds on an Ollama model's effective context window.

The probe makes no assumption about Ollama's default context. It sends
progressively larger prompts through the OpenAI-compatible endpoint and uses
reported prompt-token usage to detect rejection or silent truncation.

Usage: python3 t5c_ollama_numctx.py [base_host] [model]
Environment: OCTOPUS_OLLAMA_PROBE_SIZES=4000,8000,16000,24000,32000
"""
import json
import os
import sys
import time
import urllib.request

HOST = sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1:11434"
MODEL = sys.argv[2] if len(sys.argv) > 2 else "qwen2.5:3b"
SIZES = [int(x) for x in os.getenv(
    "OCTOPUS_OLLAMA_PROBE_SIZES", "4000,8000,16000,24000,32000"
).split(",") if x.strip()]
if not SIZES or any(size <= 0 for size in SIZES):
    raise SystemExit("probe sizes must be positive integers")

# This phrase is approximately ten tokens per repetition for the target model.
# The decision uses Ollama's reported count, not this approximation.
PARA = "The quick brown fox jumps over the lazy dog. "


def call(payload, timeout=900):
    request = urllib.request.Request(
        f"http://{HOST}/v1/chat/completions",
        data=json.dumps(payload).encode(), method="POST",
        headers={"content-type": "application/json"},
    )
    started = time.time()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return json.loads(response.read().decode()), time.time() - started
    except Exception as exc:
        return {"__error__": str(exc)[:160]}, time.time() - started


print(f"host={HOST}  model={MODEL}")
print("progressive OpenAI-compatible endpoint probe:")
lower_bound = 0
first_limited = None

for requested in SIZES:
    repeats = max(1, requested // 10)
    payload = {
        "model": MODEL,
        "max_tokens": 8,
        "messages": [
            {"role": "system", "content": PARA * repeats},
            {"role": "user", "content": "Reply with one word: ok"},
        ],
    }
    data, seconds = call(payload)
    if "__error__" in data:
        print(f"  requested ~{requested:>6,} tokens: ERROR {data['__error__']} ({seconds:.0f}s)")
        first_limited = requested
        break
    seen = (data.get("usage") or {}).get("prompt_tokens")
    if not isinstance(seen, int):
        print(f"  requested ~{requested:>6,} tokens: no prompt-token count ({seconds:.0f}s)")
        first_limited = requested
        break
    # Allow for the deliberately approximate text-to-token ratio. A sharp drop
    # below 90% is evidence of truncation; accepted sizes establish a lower
    # bound rather than an unjustified exact maximum.
    if seen < requested * 0.90:
        print(f"  requested ~{requested:>6,} tokens: model saw {seen:>7,} - truncated ({seconds:.0f}s)")
        first_limited = requested
        break
    print(f"  requested ~{requested:>6,} tokens: model saw {seen:>7,} - accepted ({seconds:.0f}s)")
    lower_bound = max(lower_bound, seen)

if lower_bound:
    print(f"measured accepted lower bound: {lower_bound:,} prompt tokens")
else:
    print("measured accepted lower bound: none established")
if first_limited is None:
    print(f"first truncation/failure: not observed through ~{max(SIZES):,} requested tokens")
else:
    print(f"first truncation/failure: at or before ~{first_limited:,} requested tokens")
if lower_bound == 0:
    raise SystemExit(1)
