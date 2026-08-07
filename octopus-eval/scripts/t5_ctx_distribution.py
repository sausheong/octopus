#!/usr/bin/env python3
"""What fraction of real Terra turns fit inside a local model's context window?

context at a turn = input_tokens + cache_creation_input_tokens + cache_read_input_tokens
Deduped on message.id (transcripts repeat entries).
"""
import json
import glob
import os

ROOT = os.path.expanduser("~/.claude/projects")
WINDOWS = [8192, 32768, 131072]  # typical local-model context sizes

seen = set()
ctxs = []
files = glob.glob(os.path.join(ROOT, "**", "*.jsonl"), recursive=True)
for fp in files:
    try:
        with open(fp, "r", errors="ignore") as f:
            for line in f:
                if '"usage"' not in line:
                    continue
                try:
                    d = json.loads(line)
                except Exception:
                    continue
                msg = d.get("message") or {}
                mid = msg.get("id")
                u = msg.get("usage") or {}
                if not mid or mid in seen or not u:
                    continue
                seen.add(mid)
                ctx = (u.get("input_tokens", 0) or 0) \
                    + (u.get("cache_creation_input_tokens", 0) or 0) \
                    + (u.get("cache_read_input_tokens", 0) or 0)
                if ctx > 0:
                    ctxs.append(ctx)
    except Exception:
        continue

ctxs.sort()
n = len(ctxs)
print(f"transcript files scanned : {len(files):,}")
print(f"deduped assistant turns  : {n:,}")
if n:
    print(f"mean context             : {sum(ctxs)//n:,} tokens")
    print(f"median                   : {ctxs[n//2]:,}")
    print(f"p90                      : {ctxs[int(n*0.90)]:,}")
    print(f"p99                      : {ctxs[int(n*0.99)]:,}")
    print()
    for w in WINDOWS:
        fit = sum(1 for c in ctxs if c <= w)
        print(f"  turns fitting in {w:>7,} ctx : {fit:>8,} / {n:,}  = {fit/n:6.2%}")
