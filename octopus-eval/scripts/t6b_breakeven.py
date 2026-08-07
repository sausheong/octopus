#!/usr/bin/env python3
"""T6b: what "break-even after N turns" actually means, as a cumulative cost curve.

Scenario: you are mid-session on model A with a warm prompt cache. Octopus
switches you to cheaper model B. Switching abandons the warm cache, so the
first turn on B must re-write it (1.25x input rate, Anthropic's 5-minute
write multiplier). Every turn after that reads B's cache at 0.10x.

Question the curve answers: after how many turns has the cheaper per-turn rate
repaid the one-off cost of rebuilding the cache?
"""
CACHE_READ = 0.10
CACHE_WRITE_5M = 1.25

RATE_IN = {"opus": 15.0, "sonnet": 3.0, "haiku": 1.0}
RATE_OUT = {"opus": 75.0, "sonnet": 15.0, "haiku": 5.0}

CTX = 380_000      # Terra measured average context
OUT_TOKENS = 2_000  # typical assistant turn


def turn_cost(model, multiplier, include_output=True):
    c = CTX / 1e6 * RATE_IN[model] * multiplier
    if include_output:
        c += OUT_TOKENS / 1e6 * RATE_OUT[model]
    return c


def curve(frm, to, turns=10, include_output=True):
    stay_per = turn_cost(frm, CACHE_READ, include_output)
    switch_first = turn_cost(to, CACHE_WRITE_5M, include_output)
    switch_rest = turn_cost(to, CACHE_READ, include_output)
    rows = []
    for n in range(1, turns + 1):
        stay = stay_per * n
        sw = switch_first + switch_rest * (n - 1)
        rows.append((n, stay, sw, stay - sw))
    return stay_per, switch_first, switch_rest, rows


def report(frm, to, include_output=True):
    tag = "input+output" if include_output else "input only"
    stay_per, sw1, swr, rows = curve(frm, to, 10, include_output)
    print(f"\n=== stay on {frm} (warm cache)  vs  switch to {to}   [{tag}] ===")
    print(f"  staying, per turn          : ${stay_per:.4f}")
    print(f"  switch turn (rebuild cache): ${sw1:.4f}   <- one-off penalty")
    print(f"  every turn after the switch: ${swr:.4f}")
    print(f"  penalty paid on switch turn: ${sw1 - stay_per:+.4f}")
    print(f"  saved on each later turn   : ${stay_per - swr:.4f}")
    print()
    print(f"  {'turns':>5} {'cum. stay':>11} {'cum. switch':>12} {'switch is':>14}")
    crossed = None
    for n, stay, sw, diff in rows:
        mark = "cheaper" if diff > 0 else "MORE EXPENSIVE"
        if diff > 0 and crossed is None:
            crossed = n
        print(f"  {n:>5} {stay:>11.4f} {sw:>12.4f} {'$%+.4f' % diff:>10} {mark}")
    saving = stay_per - swr
    if sw1 <= stay_per:
        exact = 1.0
        crossed = 1
        print("\n  -> switching is cheaper immediately on the switch turn "
              "(0 further turns; exact crossover at 1.00 turn)")
    elif saving <= 0:
        exact = float("inf")
        print("\n  -> switching never breaks even; the target is not cheaper per warm turn")
    else:
        exact = (sw1 - stay_per) / saving + 1
        print(f"\n  -> switching is cheaper from turn {crossed} onward "
              f"(exact crossover at {exact:.2f} turns)")
    return exact


print("=" * 74)
print("T6b — what 'break-even after N turns' means")
print("=" * 74)

e1 = report("opus", "sonnet", include_output=True)
e2 = report("opus", "sonnet", include_output=False)

print("\n" + "=" * 74)
print("Reading the number")
print("=" * 74)
print(f"""
  The switch turn costs MORE than staying would have, because the cache must be
  rebuilt on the new model. That one-off penalty is repaid by the cheaper turns
  that follow.

  input+output : switching pays for itself after {e1:.2f} turns
  input only   : switching pays for itself after {e2:.2f} turns

  It does NOT mean "5 turns on sonnet == 3.1 turns on opus". The comparison is
  always over the SAME number of turns: run both policies for N turns and see
  which cumulative total is lower. Below the crossover, switching is a loss;
  above it, a gain that keeps growing.

  Caveat: this is a pure price model. It assumes the two models take the same
  number of turns to finish the work. If the cheaper model needs even one extra
  turn on a long-context task, the saving is erased -- quality is not modelled
  here, and Terra's own history is the better guide to that.
""")
