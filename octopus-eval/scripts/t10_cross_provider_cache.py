#!/usr/bin/env python3
"""T10: do non-Anthropic models on AIP litellm cache, and does litellm report it?

Anthropic caching is explicit (cache_control markers). OpenAI and Gemini cache
automatically with no marker. All three report differently, so this probes the
OpenAI-compatible /v1/chat/completions shape uniformly and dumps whatever usage
accounting comes back.

Sends the same >1024-token prompt N times per model (T9 showed a cache write is
not readable ~3s later, so we repeat and watch for a hit to appear).
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
BASE = _BASE + "/v1/chat/completions"
KEY = _KEY
REPEATS = int(os.environ.get("REPEATS", "3"))

# Override for a different gateway: EVAL_MODELS="a,b,c"
MODELS = [m.strip() for m in os.environ.get("EVAL_MODELS", ",".join([
    "gpt-5.4-mini",
    "gpt-5.4",
    "gemini-3.5-flash-global",
    "gemini-3.1-flash-lite-preview-global",
    _MODEL,                       # control: known to cache
    "DeepSeek-V3-0324",
])).split(",") if m.strip()]

PARA = ("The quick brown fox jumps over the lazy dog while the diligent engineer "
        "measures token accounting behaviour under controlled conditions. ")
FILLER = PARA * 250   # ~6k tokens, over every provider's minimum


def post(model, user_text):
    payload = {
        "model": model,
        "max_tokens": 16,
        "messages": [
            {"role": "system", "content": "You are a measurement fixture. " + FILLER},
            {"role": "user", "content": user_text},
        ],
    }
    req = urllib.request.Request(BASE, data=json.dumps(payload).encode(), method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("Authorization", f"Bearer {KEY}")
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            d = json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return {"error": e.read().decode()[:160], "status": e.code}
    except Exception as e:
        return {"error": str(e)[:160]}
    ms = int((time.time() - t0) * 1000)
    u = d.get("usage", {}) or {}
    ptd = u.get("prompt_tokens_details") or {}
    return {
        "ms": ms,
        "prompt_tokens": u.get("prompt_tokens"),
        "cached_tokens": ptd.get("cached_tokens"),
        "cache_creation": u.get("cache_creation_input_tokens"),
        "cache_read": u.get("cache_read_input_tokens"),
        "completion": u.get("completion_tokens"),
        "extra_usage_keys": sorted(k for k in u
                                   if k not in ("prompt_tokens", "completion_tokens",
                                                "total_tokens", "prompt_tokens_details")),
    }


results = {}
for m in MODELS:
    print(f"\n=== {m} ===")
    rows = []
    for i in range(REPEATS):
        r = post(m, "Reply with the single word: ok")
        rows.append(r)
        if "error" in r:
            print(f"  run{i+1}: ERROR {r.get('status','')} {r['error'][:110]}")
            break
        print(f"  run{i+1}: prompt={r['prompt_tokens']} cached={r['cached_tokens']} "
              f"anthropic_read={r['cache_read']} ms={r['ms']} extra={r['extra_usage_keys']}")
        time.sleep(4)
    results[m] = rows

print("\n\n=== SUMMARY: does the model cache on this path? ===")
for m, rows in results.items():
    ok = [r for r in rows if "error" not in r]
    if not ok:
        print(f"  {m:44s} UNAVAILABLE")
        continue
    best = max((r.get("cached_tokens") or 0) for r in ok)
    best_anthropic = max((r.get("cache_read") or 0) for r in ok)
    hit = max(best, best_anthropic)
    verdict = f"CACHES (max {hit:,} tok)" if hit > 0 else "no cache observed"
    print(f"  {m:44s} {verdict}")

with open("t10_results.json", "w") as f:
    json.dump(results, f, indent=2)
