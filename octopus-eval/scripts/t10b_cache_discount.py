#!/usr/bin/env python3
"""T10b: what is the cache actually WORTH, per provider, measured from cost headers?

Also tests whether Anthropic can cache through the OpenAI-compatible shape at
all (it is opt-in via cache_control markers, unlike OpenAI/Gemini automatic
caching), including litellm's OpenAI-format cache_control passthrough.
"""
import json
import os
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
BASE = _BASE + "/v1"
KEY = _KEY

PARA = ("The quick brown fox jumps over the lazy dog while the diligent engineer "
        "measures token accounting behaviour under controlled conditions. ")
FILLER = PARA * 250


def call(path, payload):
    req = urllib.request.Request(BASE + path, data=json.dumps(payload).encode(), method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("Authorization", f"Bearer {KEY}")
    req.add_header("anthropic-version", "2023-06-01")
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            d = json.loads(r.read().decode())
            cost = r.headers.get("x-litellm-response-cost-original")
    except urllib.error.HTTPError as e:
        return {"error": e.read().decode()[:200]}
    u = d.get("usage", {}) or {}
    ptd = u.get("prompt_tokens_details") or {}
    return {
        "cost": float(cost) if cost else None,
        "prompt": u.get("prompt_tokens") or u.get("input_tokens"),
        "cached": ptd.get("cached_tokens") or u.get("cache_read_input_tokens") or 0,
        "created": u.get("cache_creation_input_tokens") or 0,
    }


def oai(model, marker=False):
    sys_content = "You are a measurement fixture. " + FILLER
    if marker:
        # litellm's OpenAI-format cache_control passthrough for Anthropic models
        sys_msg = {"role": "system", "content": [
            {"type": "text", "text": sys_content,
             "cache_control": {"type": "ephemeral"}}]}
    else:
        sys_msg = {"role": "system", "content": sys_content}
    return "/chat/completions", {
        "model": model, "max_tokens": 16,
        "messages": [sys_msg, {"role": "user", "content": "Reply with one word: ok"}],
    }


def anth(model):
    return "/messages", {
        "model": model, "max_tokens": 16,
        "system": [{"type": "text", "text": "You are a measurement fixture. " + FILLER,
                    "cache_control": {"type": "ephemeral"}}],
        "messages": [{"role": "user", "content": "Reply with one word: ok"}],
    }


CASES = [
    ("gpt-5.4-mini  (openai shape, automatic)", *oai("gpt-5.4-mini")),
    ("gemini-3.5-flash (openai shape, implicit)", *oai("gemini-3.5-flash-global")),
    ("claude-haiku  (openai shape, NO marker)", *oai("claude-haiku-4-5@20251001-global")),
    ("claude-haiku  (openai shape, WITH marker)", *oai("claude-haiku-4-5@20251001-global", True)),
    ("claude-haiku  (anthropic shape, marker)", *anth("claude-haiku-4-5@20251001-global")),
]

print(f"{'case':44s} {'run':>4} {'prompt':>7} {'cached':>7} {'cost $':>11}")
out = {}
for label, path, payload in CASES:
    rows = []
    for i in range(3):
        r = call(path, payload)
        rows.append(r)
        if "error" in r:
            print(f"{label:44s} {i+1:>4} ERROR {r['error'][:60]}")
            break
        print(f"{label:44s} {i+1:>4} {r['prompt']:>7} {r['cached']:>7} {r['cost']:>11.6f}")
        time.sleep(4)
    out[label] = rows

print(f"\n{'case':44s} {'cache hit?':>11} {'cost saved':>11}")
for label, rows in out.items():
    ok = [r for r in rows if "error" not in r]
    if len(ok) < 2:
        print(f"{label:44s} {'n/a':>11}")
        continue
    cold, warm = ok[0], ok[-1]
    hit = warm["cached"] > 0
    saved = ""
    if cold["cost"] and warm["cost"]:
        pct = (1 - warm["cost"] / cold["cost"]) * 100
        saved = f"{pct:.1f}%"
    print(f"{label:44s} {('YES' if hit else 'no'):>11} {saved:>11}")

with open("t10b_results.json", "w") as f:
    json.dump(out, f, indent=2)
