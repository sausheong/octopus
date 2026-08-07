#!/usr/bin/env python3
"""T9: controlled sequence to identify the caching layer.

R1 write   : system=FILLER(+cache_control), user="alpha"
R2 control : byte-identical to R1            -> proves the cache is live right now
R3 test    : same system prefix, user="bravo" -> does a PREFIX cache serve it?
R4 sanity  : system prefix altered            -> must miss

  R2 hit + R3 hit  => genuine PREFIX cache (Anthropic-native)
  R2 hit + R3 miss => EXACT-MATCH cache; prefix reuse is not happening
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
URL = _BASE + "/v1/messages"
KEY = _KEY

PARA = ("The quick brown fox jumps over the lazy dog while the diligent engineer "
        "measures token accounting behaviour under controlled conditions. ")
FILLER = PARA * 200


def build(user_text, sys_prefix="You are a measurement fixture. "):
    return {
        "model": MODEL, "max_tokens": 24,
        "system": [{"type": "text", "text": sys_prefix + FILLER,
                    "cache_control": {"type": "ephemeral"}}],
        "messages": [{"role": "user", "content": user_text}],
    }


def post(payload):
    req = urllib.request.Request(URL, data=json.dumps(payload).encode(), method="POST")
    req.add_header("content-type", "application/json")
    req.add_header("anthropic-version", "2023-06-01")
    req.add_header("Authorization", f"Bearer {KEY}")
    with urllib.request.urlopen(req, timeout=120) as r:
        d = json.loads(r.read().decode())
    u = d.get("usage", {})
    txt = "".join(b.get("text", "") for b in d.get("content", []) if b.get("type") == "text")
    return {"create": u.get("cache_creation_input_tokens"),
            "read": u.get("cache_read_input_tokens"),
            "reply": txt.strip()[:20]}


steps = [
    ("R1 write   ", build("Reply with the single word: alpha")),
    ("R2 control ", build("Reply with the single word: alpha")),
    ("R3 test    ", build("Reply with the single word: bravo")),
    ("R4 sanity  ", build("Reply with the single word: alpha", "DIFFERENT PREFIX. ")),
]

res = {}
for label, payload in steps:
    r = post(payload)
    res[label.strip()] = r
    print(f"  {label} create={str(r['create']):>6}  read={str(r['read']):>6}  reply={r['reply']!r}")
    time.sleep(3)

print()
r2, r3, r4 = res["R2 control"], res["R3 test"], res["R4 sanity"]
live = (r2["read"] or 0) > 0
prefix = (r3["read"] or 0) > 0
print(f"  cache live at test time (R2 hit) : {live}")
print(f"  prefix reused on new suffix (R3) : {prefix}")
print(f"  altered prefix missed (R4)       : {(r4['read'] or 0) == 0}")
print()
if live and prefix:
    print("  => PREFIX cache. Anthropic-native prompt caching.")
elif live and not prefix:
    print("  => EXACT-MATCH only. Not prefix reuse; a wrapper cache is likely.")
else:
    print("  => cache not live; result inconclusive, re-run.")
