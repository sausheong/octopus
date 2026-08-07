#!/usr/bin/env python3
"""T6: what can a router actually save on Terra's measured workload?

Uses Octopus's own cache multipliers (router.go:161-163) and Terra's measured
context size (docs/cost-efficiency-review-2026-07-25.md).
"""
CACHE_READ = 0.10
CACHE_WRITE_5M = 1.25

RATES = {  # USD per 1M input tokens
    "opus": 15.0,
    "sonnet": 3.0,
    "haiku": 1.0,
}

TERRA_AVG_CTX = 380_000  # measured average controller context


def input_cost(model, tokens, multiplier):
    return tokens / 1_000_000 * RATES[model] * multiplier


print(f"Terra measured average controller context: {TERRA_AVG_CTX:,} tokens\n")

print("Per-turn INPUT cost at that context:")
for m in RATES:
    cached = input_cost(m, TERRA_AVG_CTX, CACHE_READ)
    fresh = input_cost(m, TERRA_AVG_CTX, CACHE_WRITE_5M)
    print(f"  {m:7s} cached-read ${cached:7.4f}   cache-write ${fresh:7.4f}")

print("\nCost of ONE model switch (abandon warm cache, rewrite on new model):")
for frm in ("opus", "sonnet"):
    for to in ("sonnet", "haiku"):
        if frm == to:
            continue
        stay = input_cost(frm, TERRA_AVG_CTX, CACHE_READ)
        switch = input_cost(to, TERRA_AVG_CTX, CACHE_WRITE_5M)
        per_turn_saving = stay - input_cost(to, TERRA_AVG_CTX, CACHE_READ)
        ratio = switch / stay
        print(f"  {frm} -> {to}: switch turn ${switch:.4f} vs stay ${stay:.4f} "
              f"({ratio:.1f}x)")
        if switch <= stay:
            print("      break-even: immediate (0 further turns; switch turn is already cheaper)")
        elif per_turn_saving > 0:
            breakeven = (switch - stay) / per_turn_saving
            print(f"      break-even after {breakeven:.1f} further turns on {to}")
        else:
            print(f"      never breaks even (target is not cheaper when cached)")

print("\nCeiling check against measured spend shares:")
total = 11711.0
print(f"  window spend                     ${total:,.0f}")
for label, share in (("cache read", 0.55), ("cache write", 0.17), ("output", 0.08)):
    print(f"  {label:16s} {share:5.0%}  ${total*share:8,.0f}")
print(f"\n  A router changes the PRICE of tokens, never the COUNT.")
print(f"  T3 measured: at {TERRA_AVG_CTX:,} ctx the cache-aware scorer never")
print(f"  leaves the cached model -> realised arbitrage in steady state = $0.")
