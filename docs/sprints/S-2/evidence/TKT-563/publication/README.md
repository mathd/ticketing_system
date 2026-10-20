# S-2 report publication evidence

The report source and HTML are in docs/sprints/S-2/. The manifest binds each archived receipt to
its exact original bytes. Files larger than 50 KB use gzip. Read one with gzip -dc <path>.
The contentSha256 value is the SHA-256 of the decompressed bytes. sha256 is the archive-file hash.

The packet, four area records and final report check retain their board identities. The final
report check references the documentation area record. Incomplete inspections and failed attempts
remain in the board history. The first count diagnostic is archived beside the later diagnostics;
its intended summary failure also had an unexpected wire-shape failure. The final eight restored
runs are in the candidate-restored-r2 result and combined-check-map.

The renderer script, template and labels are already committed under
.claude/skills/sdlc-sprint-review/. Their hashes match the start pins. Reproduce the report from its
commit with:

    python3 -B .claude/skills/sdlc-sprint-review/scripts/render_report.py --input docs/sprints/S-2/report.en.json --output /tmp/S-2-report-reproduced.html

Use a new output path. Compare the returned sourceDigest and htmlDigest with the render receipt.
The report records product-candidate checks. Local and hosted publication checks and post-merge
checks are later observations on the board; they are not fabricated into this immutable receipt.

Narrow tables can require horizontal scrolling. Print CSS was inspected; pagination was not
executed. The report has no scripts or external resources. Loaded content remained readable
after the local server stopped; a cold offline reload was not executed.
