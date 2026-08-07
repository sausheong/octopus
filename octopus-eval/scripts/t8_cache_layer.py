#!/usr/bin/env python3
"""T8: is the cache Anthropic's native prompt cache, or a litellm response cache?

Test A — PREFIX vs EXACT-MATCH:
  Send the same cacheable system prefix twice with a DIFFERENT user suffix.
  Anthropic prompt cache  -> caches the PREFIX  -> cache_read > 0, and the two
                             replies differ (real inference each time).
  litellm response cache  -> needs an EXACT match -> cache_read == 0 (miss),
                             or an identical canned reply.

Test B — DOES KEY ORDER MATTER AT ALL:
  Two requests identical except tool input_schema.properties ordering, sent
  DIRECT (no Octopus). A hit means reordering is harmless on this path,
  independent of Octopus.
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
MODEL = _MODEL
LITELLM = _BASE + "/v1/messages"
KEY = _KEY

PARA = ("The quick brown fox jumps over the lazy dog while the diligent engineer "
        "measures token accounting behaviour under controlled conditions. ")
FILLER = PARA * 200

TOOL_ORDER_1 = ["zebra", "apple", "mango"]
TOOL_ORDER_2 = ["apple", "mango", "zebra"]

TYPES = {"zebra": "string", "apple": "number", "mango": "boolean"}


def tool(order):
    return [{
        "name": "probe_tool",
        "description": "ordering probe",
        "input_schema": {
            "type": "object",
            "properties": {k: {"type": TYPES[k]} for k in order},
            "required": ["zebra", "apple"],
        },
    }]


def build(user_text, tools=None, with_filler=True):
    body = {
        "model": MODEL,
        "max_tokens": 24,
        "system": [{
            "type": "text",
            "text": "You are a measurement fixture. " + (FILLER if with_filler else ""),
            "cache_control": {"type": "ephemeral"},
        }],
        "messages": [{"role": "user", "content": user_text}],
    }
    if tools:
        body["tools"] = tools
    return body


def post(payload):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(LITELLM, data=data, method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("anthropic-version", "2023-06-01")
    req.add_header("Authorization", f"Bearer {KEY}")
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=120) as r:
        raw = r.read().decode()
        hdrs = dict(r.headers)
    ms = int((time.time() - t0) * 1000)
    d = json.loads(raw)
    u = d.get("usage", {})
    text = "".join(b.get("text", "") for b in d.get("content", []) if b.get("type") == "text")
    cache_hdrs = {k: v for k, v in hdrs.items()
                  if "cache" in k.lower() or "litellm" in k.lower()}
    return {
        "ms": ms,
        "create": u.get("cache_creation_input_tokens"),
        "read": u.get("cache_read_input_tokens"),
        "in": u.get("input_tokens"),
        "reply": text.strip()[:60],
        "id": d.get("id", "")[:22],
        "cache_headers": cache_hdrs,
    }


print("=== TEST A: same cached prefix, DIFFERENT user suffix ===")
a1 = post(build("Reply with the single word: alpha"))
print("  req1:", json.dumps(a1))
time.sleep(3)
a2 = post(build("Reply with the single word: bravo"))
print("  req2:", json.dumps(a2))
print()
if a2["read"] and a2["read"] > 0 and a1["reply"] != a2["reply"]:
    print("  -> ANTHROPIC PREFIX CACHE: cache_read>0 while replies differ.")
    print("     A litellm response cache cannot do this (needs exact match).")
elif a2["read"] in (0, None):
    print("  -> prefix did NOT cache on a differing suffix; not a prefix cache.")
else:
    print("  -> ambiguous: identical replies; could be a response cache.")

print("\n=== TEST B: tool property ORDER differs, sent DIRECT (no Octopus) ===")
b1 = post(build("Reply with the single word: charlie", tools=tool(TOOL_ORDER_1)))
print(f"  req1 (order {TOOL_ORDER_1}):", json.dumps(b1))
time.sleep(3)
b2 = post(build("Reply with the single word: charlie", tools=tool(TOOL_ORDER_2)))
print(f"  req2 (order {TOOL_ORDER_2}):", json.dumps(b2))
print()
if b2["read"] and b2["read"] > 0:
    print("  -> reordering tool properties did NOT break the cache on this path.")
else:
    print("  -> reordering tool properties DID break the cache (read=0).")
