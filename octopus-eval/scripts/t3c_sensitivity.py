#!/usr/bin/env python3
"""T3c: how much does production absolute per-turn scorer retention depend
on the hand-entered quality/speed scalars?

Octopus's `quality` and `speed` are unvalidated guesses (validated only as
[0,1]; its own example config calls them "estimates; tune"). This samples
randomised catalogs to see how often the scorer retains a fully-cached
expensive model, and decomposes which variable drives the answer.

Fixed at real values: list prices, Octopus's 0.10/1.25 multipliers, 380k
context, 2k output, and the production absolute scoring formula. Only quality,
speed and the weight vector vary — i.e. exactly the inputs that are guesses.
The legacy relative result is retained as an explicitly labelled comparison.

Pure arithmetic: no network, no API key, fully portable.
"""
import random
import sys

SEED = int(sys.argv[1]) if len(sys.argv) > 1 else 20260805
N = int(sys.argv[2]) if len(sys.argv) > 2 else 200_000
random.seed(SEED)

IN, OUT = 380_000, 2_000
COVERAGE = 0.99999  # measured median across real Terra turns


def request_costs(f=COVERAGE):
    m = 1 - f + f * 0.10                       # blended multiplier, cached opus
    c_o = IN / 1e6 * 15 * m + OUT / 1e6 * 75   # cached opus
    c_s = IN / 1e6 * 3 * 1.25 + OUT / 1e6 * 15  # fresh sonnet must WRITE its cache
    return c_o, c_s


def retains(q_o, q_s, sp_o, sp_s, wq, wc, ws, f=COVERAGE):
    """Does the production absolute scorer keep the cached model?"""
    c_o, c_s = request_costs(f)
    # Production absolute mode keeps catalogue quality/speed on their stable
    # 0..1 scale and prices request cost against a $0.10 reference.
    cn = {"o": 1 / (1 + c_o / 0.10), "s": 1 / (1 + c_s / 0.10)}
    so = wq * q_o + wc * cn["o"] + ws * sp_o
    ss = wq * q_s + wc * cn["s"] + ws * sp_s
    return so >= ss


def retains_legacy(q_o, q_s, sp_o, sp_s, wq, wc, ws, f=COVERAGE):
    """Historical catalogue-relative compatibility scorer."""
    c_o, c_s = request_costs(f)
    inv = {"o": 1 / c_o, "s": 1 / c_s}
    mx = max(inv.values())
    cn = {k: (v / mx) * 0.5 for k, v in inv.items()}   # paid models capped at 0.5
    qmx, smx = max(q_o, q_s), max(sp_o, sp_s)
    so = wq * (q_o / qmx) + wc * cn["o"] + ws * (sp_o / smx)
    ss = wq * (q_s / qmx) + wc * cn["s"] + ws * (sp_s / smx)
    return so >= ss


def sample():
    q_s = random.uniform(0.55, 0.95)
    q_o = random.uniform(q_s, 1.0)      # opus quality >= sonnet
    sp_o = random.uniform(0.1, 0.9)
    sp_s = random.uniform(sp_o, 1.0)    # sonnet speed >= opus
    wq, wc, ws = (random.random() for _ in range(3))
    t = wq + wc + ws
    return q_o, q_s, sp_o, sp_s, wq / t, wc / t, ws / t


S = [sample() for _ in range(N)]
R = [retains(*s) for s in S]
RL = [retains_legacy(*s) for s in S]
print(f"seed={SEED}  n={N:,}  coverage={COVERAGE}")
print(f"production absolute per-turn retain rate: {sum(R)/N:.2%}")
print(f"legacy relative per-turn retain rate:      {sum(RL)/N:.2%}\n")


def band(name, key, edges):
    print(f"  retain rate by {name}:")
    for lo, hi in zip(edges, edges[1:]):
        sel = [r for s, r in zip(S, R) if lo <= key(s) < hi]
        if sel:
            print(f"    {lo:5.2f}-{hi:<5.2f}  {sum(sel)/len(sel):6.1%}   (n={len(sel):,})")
    print()


band("SPEED weight",   lambda s: s[6], [0, .1, .2, .3, .4, .5, .7, 1.01])
band("COST weight",    lambda s: s[5], [0, .1, .2, .3, .4, .5, .7, 1.01])
band("QUALITY weight", lambda s: s[4], [0, .1, .2, .3, .4, .5, .7, 1.01])
band("speed GAP (sonnet-opus)",   lambda s: s[3] - s[2], [0, .1, .2, .3, .5, .9])
band("quality GAP (opus-sonnet)", lambda s: s[0] - s[1], [0, .05, .1, .2, .45])

print("  Reading: these are production absolute per-turn scoring results; the")
print("  catalogue-relative compatibility result is shown only for comparison.")
print("  Amortized routing makes the final retain/switch choice using expected")
print("  cost to completion, thresholds and confidence after suitability scoring.")
