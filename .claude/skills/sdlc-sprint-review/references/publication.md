# Verification and publication of the release report

The release report lives in this repository, under `docs/sprints/<S-n>/`. It is not a board
attachment. The board keeps only a pointer to it, in a record. The report is the closure task's
deliverable, so it lands like any ticket's: on the task's branch and PR, merged into the default
branch (`config.code.defaultBranch`) only after `check` passes on the PR head
(`sdlc-ticket/SKILL.md` § Hard rules). The merged PR is also what lets the board move the task to
`PO Review`.

Get the sprint and the closure task from `sdlc-sprint`. The board's member list stays the source of
the sprint's work. The report only describes it.

## Files

For the sprint `S-3` and the report language `en`:

| File | Content |
|---|---|
| `docs/sprints/S-3/report.en.json` | The source data ([report-format.md](report-format.md)) |
| `docs/sprints/S-3/report.en.html` | The rendered report |
| `docs/sprints/S-3/evidence/<name>` | Optional. A receipt that a board comment cannot hold, such as a gate verdict or a trace |

There is one pair of files for each language. A new version replaces the files in the working tree.
Git keeps the older versions. Commit the evidence files with the report that cites them.

## Steps

1. **Assemble the source.** Write `docs/sprints/S-3/report.<lang>.json` in the report language
   (`sprints.reportLanguage`, `en` by default). Use only facts you have: the packet, the four area
   results, the check receipts and the board's data. Set `report.version`. A new candidate, new
   evidence, a factual correction or a recorded decision needs a new version. Link evidence with a
   board reference (key and digest) or with a URL that holds the full commit ID.
2. **Render to a new path outside the repository.**

   ```sh
   mkdir -p /tmp/sprint-S-3
   python3 -B .claude/skills/sdlc-sprint-review/scripts/render_report.py \
     --input docs/sprints/S-3/report.en.json --output /tmp/sprint-S-3/report.en.v1.html
   ```

   The command prints a receipt with `sourceDigest` and `htmlDigest`. Keep it. The renderer does not
   overwrite a file, so a new render needs a new output path.
3. **Check the page.** Open the HTML. Check offline reading with scripts off, the desktop and the
   narrow width, the light and the dark theme, and print. Check the visible text, the tables, the
   links inside the page, the chosen language, the evidence links and the states of the candidate and
   of the decision. The renderer's tests do not prove any of this for one report.
4. **Check the facts.** Ask for a bounded independent check of the final source and of the rendered
   page against the packet (`references/documentation.md`). Do not label narrative as approved if no
   one reviewed it.
5. **Place the files.** Copy the HTML to `docs/sprints/S-3/report.<lang>.html`. Stage the paths by
   name, never with `git add -A`:

   ```sh
   git add docs/sprints/S-3/report.en.json docs/sprints/S-3/report.en.html
   ```

   Run the local gate as `sdlc-ticket/SKILL.md` § Transitions step 4 describes, and read its verdict
   file. Do not edit a file while the gate runs.
6. **Commit, push, merge.** Commit on the closure task's branch. Write no AI attribution. The
   message gives the reason: the sprint, the report version, the readiness and the candidate. Push,
   open or update the task's PR, and merge it only after `check` passes on its head, following
   `sdlc-ticket/SKILL.md` (pre-merge push check, no host bypass). If a push times out, read the remote
   (`git ls-remote origin <branch>`) before you push again.
7. **Read the commit back.** Record the commit ID that holds the report on the default branch after
   the merge. A squash merge makes a new ID: use that one. Then compare the committed bytes with the
   receipt:

   ```sh
   git show <commit>:docs/sprints/S-3/report.en.html | sha256sum
   git show <commit>:docs/sprints/S-3/report.en.json | sha256sum
   ```

   The two digests must equal `htmlDigest` and `sourceDigest`. If they differ, something rewrote the
   files, for example a formatter. Do not record the renderer's digests. Find the cause and fix it
   before you publish.
8. **Post the record.** Post a `report-artifact` record on the sprint note and on the closure task
   (`sdlc-sprint/references/records.md`):

   ```json
   {"sprint": "S-3", "closureTask": "TKT-512", "reportVersion": 1, "language": "en",
    "candidate": "<full commit ID>", "commit": "<commit that holds the report>",
    "path": "docs/sprints/S-3/report.en.html", "sourcePath": "docs/sprints/S-3/report.en.json",
    "htmlDigest": "<64 hex>", "sourceDigest": "<64 hex>", "approval": "pending",
    "renderer": "python3 -B .claude/skills/sdlc-sprint-review/scripts/render_report.py ...",
    "verification": "<what you checked in step 3 and step 4>", "supersedes": []}
   ```

   `approval` is the state that the report shows. `supersedes` lists the commits that held earlier
   versions. This record is the current report pointer. A missing record, or a read-back that did not
   match, is an incomplete publication. It is not a ready hand-off.
9. **Return to `sdlc-sprint`.** Give it the commit, the path, the two digests, the candidate, the
   four area results, the readiness, the blockers and the choices that the human must make.

Keep the report's readiness apart from the human approval and from the tag. This skill does not
publish a tag and does not close the sprint.

## New versions

- A changed candidate or a material input needs the affected reviews again and a new report version. A
  report update never inherits the approval of its new digest.
- An optional status edition records the human's decision on the earlier report. It is a new version
  and a new commit. Keep the digest and the commit of the approved report as the release target.
  Mark the difference in the page (the `approval_prior_note` label does it). Commit a status edition
  after the tag is verified. Without it, the repository copy keeps reading `pending`.
- Never amend, squash, rebase away or force-push a commit that holds a report version that a human
  approval names. The approval's `reportUrl` pins that commit. Later versions are new commits.
- A new fact or a candidate change needs the affected review and a new human approval.
- Do not get new credentials, and do not use another client, to get around a refused push.
