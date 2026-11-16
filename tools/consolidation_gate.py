#!/usr/bin/env python3
"""Consolidation gate. Usage: consolidation_gate.py [base-ref]   (default: HEAD)
Judges only what this change touched. Exit 0 = clean, 1 = FAIL lines to fix, 2 = gate could not run."""
import json, pathlib, re, subprocess, sys, tempfile

BASE = sys.argv[1] if len(sys.argv) > 1 else "HEAD"
MAX_LEN, MAX_CCN, MAX_FILE, MIN_TOKENS = 80, 15, 600, 50
SRC = re.compile(r"\.(go|py|ts|tsx|js|jsx|c|cc|cpp|h|hpp|cs|dart|rs|java|kt)$")
STALE = re.compile(r"\w*(V[2-9]|_v[2-9]|Legacy|_legacy|_old|_new|Deprecated|_copy|_backup)\b")

def sh(*cmd):
    return subprocess.run(cmd, capture_output=True, text=True).stdout

try:
    import lizard
except ImportError:
    print("gate cannot run: pip install lizard"); sys.exit(2)

def funcs(path, code):
    return {f.name: (f.nloc, f.cyclomatic_complexity, f.start_line)
            for f in lizard.analyze_file.analyze_source_code(path, code).function_list}

# What did this change touch? file -> set of added/modified line numbers
untracked = [f for f in sh("git", "ls-files", "--others", "--exclude-standard").splitlines() if SRC.search(f)]
touched, added_text, plus, minus = {}, [], 0, 0
for f in untracked:
    lines = pathlib.Path(f).read_text(errors="ignore").splitlines()
    touched[f] = set(range(1, len(lines) + 1)); added_text += [(f, l) for l in lines]; plus += len(lines)
cur = None
for line in sh("git", "diff", "-U0", "--diff-filter=AMR", BASE).splitlines():
    if line.startswith("+++ "):
        cur = line[6:] if line.startswith("+++ b/") and SRC.search(line) else None
        if cur: touched.setdefault(cur, set())
    elif cur and line.startswith("@@"):
        start, _, count = re.search(r"\+(\d+)(,(\d+))?", line).groups()
        touched[cur] |= set(range(int(start), int(start) + int(count or 1)))
    elif cur and line.startswith("+"):
        added_text.append((cur, line[1:])); plus += 1
    elif cur and line.startswith("-") and not line.startswith("---"):
        minus += 1
if not touched:
    print("gate: no source changes"); sys.exit(0)

fails, warns = [], []

# 1. Duplication: a clone counts against you if either side overlaps lines you wrote
with tempfile.TemporaryDirectory() as out:
    sh("npx", "--yes", "jscpd", ".", "--min-tokens", str(MIN_TOKENS), "--ignore-identifiers", "--similarity",
       "--reporters", "json", "--output", out)
    report = pathlib.Path(out, "jscpd-report.json")
    if not report.exists():
        print("gate cannot run: jscpd produced no report (is Node installed?)"); sys.exit(2)
    for d in json.loads(report.read_text())["duplicates"]:
        a, b = d["firstFile"], d["secondFile"]
        if any(touched.get(s["name"], set()) & set(range(s["start"], s["end"] + 1)) for s in (a, b)):
            fails.append(f"DUPLICATE  ({d.get('kind', 'exact')}) {a['name']}:{a['start']}-{a['end']}  ==  {b['name']}:{b['start']}-{b['end']}")

# 2. Ratchet: no new over-limit function, no over-limit function made worse, no over-limit file grown
for f in touched:
    code = pathlib.Path(f).read_text(errors="ignore")
    old_code = "" if f in untracked else sh("git", "show", f"{BASE}:{f}")
    old = funcs(f, old_code)
    for name, (nloc, ccn, line) in funcs(f, code).items():
        o = old.get(name, (0, 0, 0))
        if (nloc > MAX_LEN and nloc > o[0]) or (ccn > MAX_CCN and ccn > o[1]):
            fails.append(f"GREW       {f}:{line} {name}: {nloc} lines (was {o[0]}, max {MAX_LEN}), complexity {ccn} (was {o[1]}, max {MAX_CCN})")
    n, was = code.count("\n"), old_code.count("\n")
    if n > MAX_FILE and n > was:
        fails.append(f"GREW       {f}: {n} lines (was {was}, max {MAX_FILE})")

# 3. Signals that need an answer rather than a fix
for f, text in added_text:
    m = STALE.search(text)
    if m: warns.append(f"PARALLEL?  {f}: '{m.group(0)}' looks like a second version living beside a first")
if plus > 150 and minus < plus * 0.1:
    warns.append(f"ADDITIVE   +{plus}/-{minus}: almost nothing was removed or reshaped. What existing code did this replace or reuse?")

for l in fails: print("FAIL", l)
for l in warns: print("WARN", l)
print(f"gate: {len(fails)} fail, {len(warns)} warn (base {BASE})")
sys.exit(1 if fails else 0)
