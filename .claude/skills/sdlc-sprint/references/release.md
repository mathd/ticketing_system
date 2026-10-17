# Release tag and sprint close

This file applies to a sprint with `release: true`. A planning-only sprint (`release: false`) has no
report, no tag and no closure task. It closes under `lifecycle.md` § Close a planning-only sprint.

Use only the candidate and the report that the named human approved. The close approval names:

- the sprint key,
- the exact candidate SHA,
- the digest of the current brief and the scope digest,
- the HTML digest of the report,
- the proposed tag,
- the permission to publish the tag and to close the sprint after the closure task is Done.

A readiness verdict from the review is never a human authorization.

## Before you create the tag

1. Check the intended remote and the repository's signing policy.
2. Check the tag name: `git check-ref-format refs/tags/RELEASE_TAG`.
3. Check the local and remote refs. A tag with that name that points to another commit is a blocker.
   Never move, replace, delete or force-push a release tag.
4. Check for workflows that a tag starts. Put their effects in the approval request.

## Create and publish the tag

Create an annotated tag at the approved SHA. Use a signed tag when the project's policy requires
one. The unsigned form is:

```sh
git tag -a RELEASE_TAG CANDIDATE_SHA -F /absolute/release-note.txt
git rev-parse refs/tags/RELEASE_TAG
git rev-parse 'refs/tags/RELEASE_TAG^{commit}'
```

The tag points at the candidate. The report commit comes after the candidate, because the report
names the candidate. So the tagged tree does not contain the report. This is expected. Never use the
current HEAD in place of the approved SHA.

Post a `release` record with `event: tag-attempt` on the sprint note and on the closure task. It
holds the local tag object ID, the annotation, the commit it resolves to and, when the policy
requires it, the result of `git verify-tag`. Do this before you push. Then push only that ref:

```sh
git push origin refs/tags/RELEASE_TAG
git ls-remote origin refs/tags/RELEASE_TAG 'refs/tags/RELEASE_TAG^{}'
```

The remote object ID and the peeled commit must equal the recorded local tag and the approved
candidate. Then post a `release` record with `event: tag-published` and `verified: true`.

If the push times out, read the remote before you push again:

- A matching object on the remote completes the publication. Do not push again.
- No local tag but a matching remote tag: fetch only that tag into its absent local ref. Check the
  recorded object ID, the candidate and the signature.
- A remote tag that holds an unknown object at the same commit: stop and ask the human. Never adopt
  it.

## Close the sprint

1. Keep the sprint `active` until the closure task is Done. The closure task is Done when its
   criteria hold: the report's PR is merged, the evidence is complete, the close approval is recorded
   and the tag is verified. In this project the pipeline accepts a ticket by moving it to Done.
2. Read the sprint again. Build the snapshot. Run `sprint_state.py close`. It must print `CLOSE`.
3. Send `POST /sprint/close` with the `scopeDigest` that the close approval names. Read the sprint
   again. Check `state: closed`, `closedAt`, and that `endScope` is the approved member list
   (the board's `endScopeMismatch` is an empty list; `sprint_state.py` reports its own derived
   boolean, which must be `false`).
4. Post a `release` record with `event: closed`.

If the close fails after the tag is published, keep the tag. Keep the sprint active. Resume from the
board's state and from the recorded publication. Do not make another tag.

If the human closes the sprint in the board view, report the difference. Name the gates that are
missing. Do not infer a release approval. Do not make the sprint again.

A tag and a close do not authorize a GitHub release or a deployment. Those are out of scope.
