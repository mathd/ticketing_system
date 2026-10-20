# TKT-552 verification evidence

The product change adds two Entry instructions. The exact tests cover the presenter, submitted 409 responses, existing 422 denials and failed final writes. All other Entry and Exit expectations remain in place.

`implementation-logs/red-v2-focused.log` is the authoritative red run. It has ten intended text failures and 583 passes on the original presenter. Its test patch matches the final four test files. The initial capturing stub did not preserve the existing routing. It was corrected, then red was repeated against the original source. Earlier logs and the first patch are retained as superseded history. The final focused run passes 593 tests. The three existing documentation checks, scanner lint and TypeScript/Vite build pass.

`verification/summary.json` records the primary checks, commands, UTC windows and hashes. Four mutants remove each new branch and each Reason line separately. Each fails at both the exact presenter assertion and the submitted App paragraph assertion. The raw logs retain those failures. Each restoration receipt confirms the original committed source hash and a clean worktree. Eight complete scanner suites then pass 1,865 tests each. The full suites include the changed safety fixtures and unchanged Exit and status-first controls. Primary lint and build also pass.

The mutation runner uses the implementation commit as its input. It writes only the presenter while no gate is active and restores that exact committed source after each mutation. The committed evidence changes no product file after those checks. Full machine reports stay in the scratch evidence directory; the raw verbose outputs and compact results are retained here.

No host Chrome verification of the new text is claimed. No write form, control or contract changed. The existing browser journey checks the rejection heading and HTTP reason. TKT-563 runs the combined host-Chrome journeys. Local full-gate and hosted-check receipts are recorded on the board against their exact later commits.
