# TKT-492 fixture ticket wait

The retained control run `run-20260927T230204973Z-390eecff` aborted with
`issued_ticket_history_invalid`. It did not save the exact history seen by the runner, so that
run does not prove why the assertion failed.

The source shows a valid timing window. Access issues tickets before it delivers them. The ticket
reader returns each ticket with its lifecycle history, while `processCompleted` calls `issue` and
then `deliver`. The old fixture wait returned when the HTTP response had the expected ticket count.
`checkoutBatch` then required each history to contain `issued` sequence 1 followed by `delivered`
sequence 2. The count can therefore be ready while a ticket has only its normal issued event.

`run.mjs` now polls under the existing 30-second wait deadline. It retries an exact issued-only
history prefix and returns only when every ticket has the exact issued-plus-delivered history. A
malformed, wrong-order, or extra event fails with the existing safe assertion label. The final
`checkoutBatch` history assertion remains in place. The runner does not repeat checkout or issue
more tickets, and this change does not alter the measured workload or thresholds.

Pure assertions used deterministic inputs for the classifier: an issued-only prefix, exact complete
history, malformed history, reversed events, and extra history. The check extracted the function
from `run.mjs`; it did not import or start the runner. The assertions are not a retained standalone
test because the requested edit scope allows only this runner and note. No runner self-check,
service, Docker, browser, listener, or network action was used.
