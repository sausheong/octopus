#!/usr/bin/env python3
"""T1: does routing through Octopus void Anthropic's prompt cache?

Sends a byte-identical, cacheable (>2048 token) request twice down each arm and
reports cache_creation_input_tokens / cache_read_input_tokens.

  arm A (direct)  : client -> litellm            [baseline: proves the prompt caches at all]
  arm B (octopus) : client -> octopus -> litellm [the question]

A cache HIT on request 2 means cache_read_input_tokens > 0.
"""
import json
import os
import sys
import time
import urllib.request

# Endpoint is configurable so this runs against any Anthropic-compatible API.
#   EVAL_BASE     base URL              (default: AIP litellm staging)
#   EVAL_MODEL    anthropic model id    (default: haiku via litellm)
#   EVAL_API_KEY  key; falls back to OPENAI_API_KEY then ANTHROPIC_API_KEY
# Direct Anthropic:
#   EVAL_BASE=https://api.anthropic.com EVAL_MODEL=claude-haiku-4-5-20251001 \
#   EVAL_API_KEY=$ANTHROPIC_API_KEY python3 <script>
_BASE = os.environ.get("EVAL_BASE", "https://litellm-stg.aip.gov.sg").rstrip("/")
_MODEL = os.environ.get("EVAL_MODEL", "claude-haiku-4-5@20251001-global")
_KEY = (os.environ.get("EVAL_API_KEY") or os.environ.get("OPENAI_API_KEY")
        or os.environ.get("ANTHROPIC_API_KEY"))
if not _KEY:
    raise SystemExit("no API key: set EVAL_API_KEY (or OPENAI_API_KEY / ANTHROPIC_API_KEY)")
MODEL_DIRECT = _MODEL
LITELLM = _BASE
OCTOPUS = os.environ.get("EVAL_OCTOPUS", "http://127.0.0.1:8787")
KEY = _KEY

# Deterministic filler, comfortably over the 2048-token minimum cacheable prefix.
# Identical bytes every run so the two requests in an arm share a cache prefix.
PARA = ("The quick brown fox jumps over the lazy dog while the diligent engineer "
        "measures token accounting behaviour under controlled conditions. ")
FILLER = (PARA * 200)


def build(model):
    return {
        "model": model,
        "max_tokens": 16,
        "system": [{
            "type": "text",
            "text": "You are a measurement fixture. " + FILLER,
            "cache_control": {"type": "ephemeral"},
        }],
        "messages": [{"role": "user", "content": "Reply with the single word: ok"}],
    }


def post(url, payload, use_bearer=True):
    body = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("anthropic-version", "2023-06-01")
    if use_bearer:
        req.add_header("Authorization", f"Bearer {KEY}")
    req.add_header("x-api-key", KEY)
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            raw = r.read().decode()
    except urllib.error.HTTPError as e:
        return {"error": e.read().decode()[:400], "status": e.code,
                "ms": int((time.time() - t0) * 1000)}
    ms = int((time.time() - t0) * 1000)
    try:
        d = json.loads(raw)
    except Exception:
        return {"error": "non-json", "raw": raw[:400], "ms": ms}
    u = d.get("usage", {})
    return {
        "ms": ms,
        "input_tokens": u.get("input_tokens"),
        "cache_creation_input_tokens": u.get("cache_creation_input_tokens"),
        "cache_read_input_tokens": u.get("cache_read_input_tokens"),
        "output_tokens": u.get("output_tokens"),
        "model": d.get("model"),
    }


def run_arm(name, url, model):
    print(f"\n=== ARM {name} -> {url} (model={model}) ===")
    results = []
    for i in (1, 2):
        r = post(url, build(model))
        results.append(r)
        print(f"  req{i}: {json.dumps(r)}")
        time.sleep(3)  # settle; well inside the 5-minute ephemeral TTL
    return {"arm": name, "url": url, "model": model, "results": results}


if __name__ == "__main__":
    arm = sys.argv[1] if len(sys.argv) > 1 else "both"
    out = []
    if arm in ("direct", "both"):
        out.append(run_arm("A-direct", f"{LITELLM}/v1/messages", MODEL_DIRECT))
    if arm in ("octopus", "both"):
        out.append(run_arm("B-octopus", f"{OCTOPUS}/v1/messages",
                           f"litellm/{MODEL_DIRECT}"))
    print("\n" + json.dumps(out, indent=2))
