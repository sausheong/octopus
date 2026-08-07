#!/usr/bin/env python3
"""Render this directory's markdown reports to a single self-contained HTML page.

No external assets or network use — the output opens straight from disk.
Usage: python3 render.py [--no-open] [out.html]
"""
import html
import re
import sys
import os
import webbrowser

HERE = os.path.dirname(os.path.abspath(__file__))
# summary.md already contains the current-run checklist. Rendering that single
# source prevents duplicated or cross-run evidence in report.html.
SOURCES = ["summary.md"]
args = sys.argv[1:]
OPEN_BROWSER = "--no-open" not in args
args = [arg for arg in args if arg != "--no-open"]
if len(args) > 1:
    raise SystemExit("usage: render.py [--no-open] [out.html]")
OUT = args[0] if args else os.path.join(HERE, "report.html")

CSS = """
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body {
  margin: 0; padding: 3rem 1.5rem 6rem;
  font: 16px/1.65 -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, sans-serif;
  background: #fbfbfa; color: #1a1a18;
}
main { max-width: 60rem; margin: 0 auto; }
h1 { font-size: 2rem; letter-spacing: -0.02em; margin: 0 0 .4rem; }
h2 { font-size: 1.35rem; letter-spacing: -0.01em; margin: 2.75rem 0 .75rem;
     padding-bottom: .4rem; border-bottom: 2px solid #e6e4df; }
h3 { font-size: 1.08rem; margin: 2rem 0 .6rem; color: #33312c; }
p, li { color: #2e2c28; }
code { font: 0.86em ui-monospace, SFMono-Regular, Menlo, monospace;
       background: #efede8; padding: .12em .38em; border-radius: 4px; }
pre { background: #14140f; color: #e8e6df; padding: 1rem 1.15rem; border-radius: 8px;
      overflow-x: auto; line-height: 1.5; }
pre code { background: none; padding: 0; color: inherit; font-size: .82rem; }
table { border-collapse: collapse; width: 100%; margin: 1.1rem 0; font-size: .92rem;
        display: block; overflow-x: auto; }
th, td { border: 1px solid #e0ddd6; padding: .5rem .7rem; text-align: left;
         vertical-align: top; }
th { background: #f2f0eb; font-weight: 600; }
tbody tr:nth-child(even) { background: #f7f6f3; }
hr { border: 0; border-top: 1px solid #e0ddd6; margin: 2.5rem 0; }
a { color: #0b5cad; }
blockquote { margin: 1rem 0; padding: .1rem 1rem; border-left: 3px solid #d8d5cd; color: #55524b; }
.src { font-size: .78rem; text-transform: uppercase; letter-spacing: .08em;
       color: #8a877f; margin: 3.5rem 0 .5rem; }
@media (prefers-color-scheme: dark) {
  body { background: #14140f; color: #e8e6df; }
  h3 { color: #cfccc3; } p, li { color: #d6d3ca; }
  h2 { border-bottom-color: #33312b; }
  code { background: #26251f; }
  th, td { border-color: #33312b; } th { background: #201f1a; }
  tbody tr:nth-child(even) { background: #1a1915; }
  hr { border-top-color: #33312b; } a { color: #7ab7f5; }
  blockquote { border-left-color: #3d3a33; color: #a29e95; }
  .src { color: #6d6a62; }
}
"""


def inline(t):
    t = html.escape(t)
    t = re.sub(r"`([^`]+)`", r"<code>\1</code>", t)
    t = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", t)
    t = re.sub(r"(?<![*\w])\*([^*]+)\*(?!\*)", r"<em>\1</em>", t)
    t = re.sub(r"\[([^\]]+)\]\(([^)]+)\)", r'<a href="\2">\1</a>', t)
    return t


def is_sep(row):
    cells = cells_of(row)
    return cells and all(re.fullmatch(r":?-{2,}:?", c) for c in cells if c != "")


def cells_of(row):
    return [
        cell.strip().replace("\\|", "|").replace("\\`", "`")
        for cell in re.split(r"(?<!\\)\|", row.strip().strip("|"))
    ]


def convert(md):
    out, i, lines = [], 0, md.split("\n")
    while i < len(lines):
        ln = lines[i]

        if ln.startswith("```"):
            i += 1
            buf = []
            while i < len(lines) and not lines[i].startswith("```"):
                buf.append(html.escape(lines[i]))
                i += 1
            i += 1
            out.append("<pre><code>" + "\n".join(buf) + "</code></pre>")
            continue

        # table: a pipe row followed by a separator row
        if ln.strip().startswith("|") and i + 1 < len(lines) and is_sep(lines[i + 1]):
            head = cells_of(ln)
            i += 2
            body = []
            while i < len(lines) and lines[i].strip().startswith("|"):
                body.append(cells_of(lines[i]))
                i += 1
            t = ["<table><thead><tr>"]
            t += [f"<th>{inline(c)}</th>" for c in head]
            t.append("</tr></thead><tbody>")
            for r in body:
                r += [""] * (len(head) - len(r))
                t.append("<tr>" + "".join(f"<td>{inline(c)}</td>" for c in r[:len(head)]) + "</tr>")
            t.append("</tbody></table>")
            out.append("".join(t))
            continue

        if re.match(r"^-{3,}\s*$", ln):
            out.append("<hr>")
            i += 1
            continue

        m = re.match(r"^(#{1,6})\s+(.*)$", ln)
        if m:
            lvl = len(m.group(1))
            out.append(f"<h{lvl}>{inline(m.group(2))}</h{lvl}>")
            i += 1
            continue

        if re.match(r"^\s*[-*]\s+", ln) or re.match(r"^\s*\d+\.\s+", ln):
            ordered = bool(re.match(r"^\s*\d+\.\s+", ln))
            tag = "ol" if ordered else "ul"
            items = []
            while i < len(lines) and (re.match(r"^\s*[-*]\s+", lines[i])
                                      or re.match(r"^\s*\d+\.\s+", lines[i])
                                      or (items and lines[i].startswith("  ") and lines[i].strip())):
                if re.match(r"^\s*[-*]\s+", lines[i]) or re.match(r"^\s*\d+\.\s+", lines[i]):
                    items.append(re.sub(r"^\s*(?:[-*]|\d+\.)\s+", "", lines[i]))
                else:
                    items[-1] += " " + lines[i].strip()
                i += 1
            out.append(f"<{tag}>" + "".join(f"<li>{inline(x)}</li>" for x in items) + f"</{tag}>")
            continue

        if ln.startswith(">"):
            out.append(f"<blockquote>{inline(ln.lstrip('> '))}</blockquote>")
            i += 1
            continue

        if not ln.strip():
            i += 1
            continue

        para = [ln]
        i += 1
        while i < len(lines) and lines[i].strip() and not re.match(
                r"^(#{1,6}\s|\||```|>|\s*[-*]\s|\s*\d+\.\s|-{3,}\s*$)", lines[i]):
            para.append(lines[i])
            i += 1
        out.append("<p>" + inline(" ".join(para)) + "</p>")
    return "\n".join(out)


parts = []
for name in SOURCES:
    path = os.path.join(HERE, name)
    if not os.path.exists(path):
        continue
    if parts:
        parts.append(f'<p class="src">{html.escape(name)}</p>')
    with open(path) as f:
        parts.append(convert(f.read()))

doc = (f'<!doctype html><html><head><meta charset="utf-8">'
       f'<meta name="viewport" content="width=device-width,initial-scale=1">'
       f'<title>Octopus evaluation — measured results</title><style>{CSS}</style></head>'
       f'<body><main>{"".join(parts)}</main></body></html>')

with open(OUT, "w") as f:
    f.write(doc)
print(f"wrote {OUT} ({len(doc):,} bytes)")
if OPEN_BROWSER:
    webbrowser.open("file://" + OUT)
