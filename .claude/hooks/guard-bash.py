#!/usr/bin/env python3
"""PreToolUse guard for Bash. Blocks (exit 2) what a tool call can prove is wrong.

Deliberately small. An earlier version tried to recognise "the gate, run
directly" by parsing shell, and lost: `sh -c`, `eval`, `nohup`, a bare newline
and a quoted paren each defeated it in turn. That enforcement moved to the
Makefile, where it is structural — `make check` IS the wrapper and the stages
refuse to start without its lock — so no spelling of the gate runs unlocked and
this file no longer tries to classify one.

What is left follows the same principle. The under-lock rule is an allowlist of
whole-command SHAPES, not a list of command names with their arguments checked:
`find -delete`, `sort -o`, `git diff --output=` and `git stash push -- list` all
read as harmless under a name-based allowlist, so there is no argument to get
right if the entire command must match one of a handful of literal forms.
"""
import json
import os
import re
import shlex
import subprocess
import sys

# The only commands that may run while the gate holds the lock. Whole-command
# matches: anything not on this list waits, including reads not thought of here.
LOG = r"\.gate\.(?:log|verdict|lock)"
PAT = r"(?:'[^']*'|[A-Za-z0-9_-]+)"
UNDER_LOCK = [
    rf"cat\s+{LOG}",
    rf"(?:head|tail)(?:\s+-n\s*\d+|\s+-\d+)?\s+{LOG}",
    rf"grep\s+(?:-[A-Za-z]+\s+)*{PAT}\s+{LOG}",
    rf"wc\s+-l\s+{LOG}",
    r"git\s+status(?:\s+--porcelain)?",
    # The documented way to wait for a background gate (AGENTS.md): never a
    # process poll, which self-matches and never returns (TKT-106).
    r"(?:while\s+\[\s+-e\s+\.gate\.lock\s+\]|until\s+\[\s+!\s+-e\s+\.gate\.lock\s+\])"
    r"\s*;\s*do\s+sleep\s+\d+\s*;\s*done",
]


# Recognising a command by its leading words needs quote context and the shell's
# separators. Quoted spans are masked first, because splitting on operators
# throws it away: the text "(gh pr merge)" passed as an ARGUMENT would otherwise
# read exactly like an invocation. This recognises the ordinary forms, including
# wrapper words and a command on its own line; it is not a shell parser, and an
# unquoted invocation inside a heredoc body reads as real. It errs toward
# refusing, which is the safe direction for a merge.
QUOTED = re.compile(r"'[^']*'|\"(?:\\.|[^\"\\])*\"")
WRAPPERS = {"env", "command", "exec", "nice", "time", "sudo", "builtin", "\\"}


def commands(cmd):
    masked = QUOTED.sub(lambda m: "Q" * len(m.group(0)), cmd)
    out = []
    for seg in re.split(r"\|\||&&|[;|&()\n]", masked):
        try:
            toks = shlex.split(seg, comments=True)
        except ValueError:
            toks = seg.split()
        while toks and ("=" in toks[0].split("/")[0] or toks[0] in WRAPPERS):
            toks = toks[1:]
        if toks:
            toks[0] = os.path.basename(toks[0])
            out.append(toks)
    return out


def block(msg):
    print(msg, file=sys.stderr)
    sys.exit(2)


def git(args, cwd):
    return subprocess.run(
        ["git", *args], cwd=cwd, capture_output=True, text=True
    ).stdout.strip()


payload = json.load(sys.stdin)
cmd = ((payload.get("tool_input") or {}).get("command") or "").strip()
cwd = payload.get("cwd") or os.getcwd()
if not cmd:
    sys.exit(0)

# 1. Nothing touches the tree while the gate reads it. An edit landing mid-run is
#    read half-written and fails for a reason that does not exist — TKT-240 spent
#    a full cycle diagnosing one.
root = git(["rev-parse", "--show-toplevel"], cwd)
lock = os.path.join(root, ".gate.lock") if root else None
if lock and os.path.exists(lock):
    if not any(re.fullmatch(shape, cmd) for shape in UNDER_LOCK):
        with open(lock) as fh:
            started = fh.read().strip()
        block(
            f"Blocked: the gate has been running since {started}. Anything not on "
            f"the wait-and-read allowlist waits for it (TKT-240): cat/head/tail/"
            f"grep/wc on .gate.log or .gate.verdict, `git status`, or "
            f"`while [ -e .gate.lock ]; do sleep 5; done`. If no gate is running: "
            f"rm {lock}"
        )

# 2. A merge must contain the work. A commit made after the last push is
#    invisible to the PR, so the merge captures a SHA that predates it — TKT-97
#    squash-merged the pre-fix version. The local remote-tracking ref is not
#    evidence (it is only as fresh as the last fetch), so ask the forge; and
#    uncommitted work is not in the PR whether or not git is tracking it yet,
#    which is how a feature whose whole implementation is still untracked would
#    otherwise merge as a documentation-only change.
if any(w[:3] == ["gh", "pr", "merge"] for w in commands(cmd)):
    dirty = git(["status", "--porcelain"], cwd)
    if dirty:
        block(
            "Blocked: the working tree has changes that are not in the PR you are "
            f"merging — commit or stash them first (TKT-97):\n{dirty}"
        )
    head = git(["rev-parse", "HEAD"], cwd)
    # A PASS is about a revision, not about a moment. The verdict records the
    # commit it tested, so a gate run before the final commit no longer counts
    # for the commit that gets merged.
    verdict_path = os.path.join(root, ".gate.verdict")
    verdict = ""
    if os.path.exists(verdict_path):
        with open(verdict_path) as fh:
            verdict = fh.read().strip()
    if not verdict.startswith("PASS "):
        block(
            "Blocked: no passing gate verdict for this tree — run `make check` "
            f"on the committed revision before merging. .gate.verdict says: "
            f"{verdict or '(missing)'}"
        )
    tested = re.search(r"head=(\S+)", verdict)
    if not tested or tested.group(1) != head:
        block(
            f"Blocked: the gate passed on {tested.group(1) if tested else '(unknown)'}, "
            f"not on HEAD ({head}). Re-run `make check` on the revision you are merging."
        )
    # A commit is not a tree. The gate can pass on a DIRTY tree at commit A; drop
    # those edits and the tree is clean at A again, with a verdict that never saw
    # it. Compare the digest the wrapper recorded against the tree in front of us,
    # using the wrapper's own definition of a digest.
    tested_tree = re.search(r"tree=(\S+)", verdict)
    now = subprocess.run(
        [os.path.join(root, "scripts", "gate.sh"), "--digest"],
        cwd=cwd, capture_output=True, text=True,
    ).stdout.strip()
    if not now:
        block("Blocked: cannot compute the current tree digest — scripts/gate.sh --digest failed.")
    if not tested_tree or tested_tree.group(1) != now:
        block(
            f"Blocked: the gate passed on tree {tested_tree.group(1) if tested_tree else '(unknown)'}, "
            f"the tree here is {now}. Same commit, different content — re-run `make check`."
        )
    num = re.search(r"\bmerge\s+(\d+)", cmd)
    view = ["gh", "pr", "view", *([num.group(1)] if num else []),
            "--json", "headRefOid", "-q", ".headRefOid"]
    got = subprocess.run(view, cwd=cwd, capture_output=True, text=True)
    oid = got.stdout.strip()
    if not oid:
        block(
            "Blocked: cannot read the PR head from the forge, so the merge cannot "
            f"be proven to contain your work (TKT-97): {got.stderr.strip()}"
        )
    if oid != head:
        block(
            f"Blocked: local HEAD ({head}) is not the PR head ({oid}). The merge "
            "would capture a SHA that predates your last commit — push, then merge "
            "(TKT-97)."
        )

# 3. No AI attribution in authored artifacts (global CLAUDE.md). The harness
#    template says the opposite, so the rule needs an enforcer, not a reminder.
if re.search(r"\bgit\b[^;|&]*\bcommit\b", cmd) and re.search(
    r"co-authored-by:\s*claude|generated with \[?claude code|claude-session:", cmd, re.I
):
    block(
        "Blocked: no Claude attribution in commit messages (global CLAUDE.md). "
        "Write it as the user would."
    )

sys.exit(0)
