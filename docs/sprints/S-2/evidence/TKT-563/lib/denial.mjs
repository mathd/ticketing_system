// Denial-copy journey (plan section 2.1).
//
// Real rendered controls submit every credential. Access decides each answer.
// The journey reads the exact local row, the probe's write history and the
// exact lifecycle rows after each step. A failed named check does not stop the
// journey, so the readbacks exist. The evidence object is filled in place, so
// every readback taken before an exception is still kept.

import { POLICY } from './fixtures.mjs';
import { awaitStorageIdle, canon, fact, poll, sanitizeDeep, scanAnswered, scanResultText, setDirection, sortById } from './kit.mjs';

export const EXIT_ALERT =
  'RejectedDo not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.Reason: exit_required.';
export const LIMIT_ALERT = 'RejectedDo not admit. No entries remain on this pass.Reason: entry_limit_reached.';

// Waits for the result section heading and returns its exact text.
async function resultFor(page, heading) {
  const shown = await poll(() => page.getByRole('heading', { name: heading, exact: true }).isVisible());
  return { shown: shown === true, text: (await scanResultText(page)) ?? '' };
}

// The local row for one occurrence once the denial reason has been written.
async function deniedRow(H, page, occurrence, reason) {
  return poll(async () => {
    const rows = await H.read(page);
    return rows.find((row) => row.occurrenceId === occurrence && row.state === 'RESOLVED' && row.localOutcome?.reason === reason) ?? null;
  });
}

// Every write the probe recorded for one occurrence, in the order the page made them.
const writesFor = (page, occurrence) =>
  page.evaluate((id) => window.__tkt558IdbProbe.events.filter((event) => event.id === id), occurrence);

// The ACTUATED writes in a write history. A write that succeeded counts, and so
// does a completed transaction. A write whose transaction later aborted still counts.
const actuatedWrites = (events) =>
  events.filter((event) => event.state === 'ACTUATED' && (event.event === 'write-success' || event.event === 'complete'));

// The wire record of one scan: status, sanitized request body and response JSON.
const wireOf = (scan) => sanitizeDeep({ status: scan.status, body: scan.body, json: scan.json });

export async function denialJourney(ctx, evidence) {
  const { H, token, fx, rb, checks, newJourneyContext, assetsDir } = ctx;
  const context = await newJourneyContext({ assetsDir });
  const page = await context.newPage();
  try {
    await H.pair(page, token);

    // Exit-required pass: Entry, denied Entry, Exit, then Entry again.
    const exitTicket = fx.create('DENY-EXIT', POLICY.multiRequiresExit);
    const first = await scanAnswered(page, H, exitTicket.credential);
    const firstShown = await resultFor(page, 'Accepted');
    checks.check(
      'DENY-EXIT-ACCEPT-ENTRY-1',
      first.status === 200 && first.json?.decision === 'accepted' && first.body.direction === 'entry' && firstShown.shown,
      `status ${first.status}`,
    );
    const firstFact = fact(first.body.occurrence_id, 'entry', first.json?.scanned_at);
    const firstReadback = { facts: rb.facts(exitTicket.ticket), lifecycle: rb.rows(exitTicket.ticket) };
    checks.check('DENY-EXIT-FACT-ENTRY-1', canon(sortById(firstReadback.facts)) === canon([firstFact]));
    const lifecycleBeforeDenial = firstReadback.lifecycle;
    evidence.exit = { firstEntry: { wire: wireOf(first), result: firstShown, readback: sanitizeDeep(firstReadback) } };

    const denied = await scanAnswered(page, H, exitTicket.credential);
    const deniedResult = await resultFor(page, 'Rejected');
    checks.check(
      'DENY-EXIT-REASON-WIRE',
      denied.status === 409 && denied.json?.decision === 'rejected' && denied.json?.reason === 'exit_required' && denied.body.direction === 'entry',
      `status ${denied.status} reason ${denied.json?.reason}`,
    );
    checks.check('DENY-EXIT-ALERT', deniedResult.shown && deniedResult.text === EXIT_ALERT, `shown ${deniedResult.shown} text ${JSON.stringify(deniedResult.text)}`);
    const exitRow = await deniedRow(H, page, denied.body.occurrence_id, 'exit_required');
    const exitIdle = await awaitStorageIdle(page);
    const exitWrites = await writesFor(page, denied.body.occurrence_id);
    const exitActuated = actuatedWrites(exitWrites);
    const exitRowsSeen = await H.read(page);
    checks.check(
      'DENY-EXIT-LOCAL-RESOLVED',
      !!exitRow &&
        exitRow.state === 'RESOLVED' &&
        exitRow.actuated === false &&
        exitActuated.length === 0 &&
        exitRow.localOutcome?.family === 'terminal' &&
        exitRow.localOutcome?.cause === 'denied' &&
        exitRow.localOutcome?.status === 409,
      `row ${JSON.stringify(sanitizeDeep(exitRow))} actuated writes ${exitActuated.length}`,
    );
    const exitLifecycleAfter = rb.rows(exitTicket.ticket);
    checks.check('DENY-EXIT-NO-LIFECYCLE', canon(exitLifecycleAfter) === canon(lifecycleBeforeDenial));
    evidence.exit.denied = {
      status: denied.status,
      reason: denied.json?.reason ?? null,
      occurrence: denied.body.occurrence_id,
      wire: wireOf(denied),
      alert: { shown: deniedResult.shown, text: deniedResult.text, expected: EXIT_ALERT },
      occurrenceRow: sanitizeDeep(exitRow),
      rowsObserved: sanitizeDeep(exitRowsSeen),
      writes: exitWrites,
      actuatedWriteCount: exitActuated.length,
      storageIdle: exitIdle,
      lifecycleBefore: lifecycleBeforeDenial,
      lifecycleAfter: exitLifecycleAfter,
    };

    await setDirection(page, 'exit');
    const exitSelected = await page.getByRole('radio', { name: 'Exit', exact: true }).isChecked();
    const exited = await scanAnswered(page, H, exitTicket.credential);
    const exitResult = await resultFor(page, 'Exit recorded');
    const exitFact = fact(exited.body.occurrence_id, 'exit', exited.json?.scanned_at);
    checks.check(
      'DENY-EXIT-ACCEPT-EXIT',
      exitSelected && exited.status === 200 && exited.json?.decision === 'accepted' && exited.body.direction === 'exit' && exitResult.shown,
      `status ${exited.status}`,
    );
    const exitReadback = { facts: rb.facts(exitTicket.ticket), lifecycle: rb.rows(exitTicket.ticket) };
    checks.check('DENY-EXIT-FACT-EXIT', canon(sortById(exitReadback.facts)) === canon(sortById([firstFact, exitFact])));
    const exitStays = await page.getByRole('radio', { name: 'Exit', exact: true }).isChecked();
    checks.check('DENY-EXIT-SELECTOR-STAYS', exitStays === true);
    evidence.exit.exit = { selected: exitSelected, wire: wireOf(exited), result: exitResult, readback: sanitizeDeep(exitReadback), selectorStays: exitStays };

    await setDirection(page, 'entry');
    const reentered = await scanAnswered(page, H, exitTicket.credential);
    const reenteredResult = await resultFor(page, 'Accepted');
    const reenterFact = fact(reentered.body.occurrence_id, 'entry', reentered.json?.scanned_at);
    checks.check(
      'DENY-EXIT-ACCEPT-ENTRY-2',
      reentered.status === 200 && reentered.json?.decision === 'accepted' && reentered.body.direction === 'entry' && reenteredResult.shown,
      `status ${reentered.status}`,
    );
    const reenterReadback = { facts: rb.facts(exitTicket.ticket), lifecycle: rb.rows(exitTicket.ticket) };
    checks.check('DENY-EXIT-FACT-ENTRY-2', canon(sortById(reenterReadback.facts)) === canon(sortById([firstFact, exitFact, reenterFact])));
    evidence.exit.reentry = { wire: wireOf(reentered), result: reenteredResult, readback: sanitizeDeep(reenterReadback) };
    evidence.exit.entryAfterExit = reentered.body.occurrence_id;

    // Count-limited pass with one allowed entry: the second entry is denied.
    const limitTicket = fx.create('DENY-LIMIT', POLICY.countLimitedOne);
    const limitFirst = await scanAnswered(page, H, limitTicket.credential);
    const limitFirstResult = await resultFor(page, 'Accepted');
    checks.check(
      'DENY-LIMIT-ACCEPT-1',
      limitFirst.status === 200 && limitFirst.json?.decision === 'accepted' && limitFirst.body.direction === 'entry' && limitFirstResult.shown,
      `status ${limitFirst.status}`,
    );
    const limitFirstReadback = { facts: rb.facts(limitTicket.ticket), lifecycle: rb.rows(limitTicket.ticket) };
    checks.check('DENY-LIMIT-FACT-1', canon(limitFirstReadback.facts) === canon([fact(limitFirst.body.occurrence_id, 'entry', limitFirst.json?.scanned_at)]));
    const limitBeforeDenial = limitFirstReadback.lifecycle;
    evidence.limit = { firstEntry: { wire: wireOf(limitFirst), result: limitFirstResult, readback: sanitizeDeep(limitFirstReadback) } };

    const limitDenied = await scanAnswered(page, H, limitTicket.credential);
    const limitResult = await resultFor(page, 'Rejected');
    checks.check(
      'DENY-LIMIT-REASON-WIRE',
      limitDenied.status === 409 && limitDenied.json?.decision === 'rejected' && limitDenied.json?.reason === 'entry_limit_reached',
      `status ${limitDenied.status} reason ${limitDenied.json?.reason}`,
    );
    checks.check('DENY-LIMIT-ALERT', limitResult.shown && limitResult.text === LIMIT_ALERT, `shown ${limitResult.shown} text ${JSON.stringify(limitResult.text)}`);
    const limitRow = await deniedRow(H, page, limitDenied.body.occurrence_id, 'entry_limit_reached');
    const limitIdle = await awaitStorageIdle(page);
    const limitWrites = await writesFor(page, limitDenied.body.occurrence_id);
    const limitActuated = actuatedWrites(limitWrites);
    const limitRowsSeen = await H.read(page);
    checks.check(
      'DENY-LIMIT-LOCAL-RESOLVED',
      !!limitRow &&
        limitRow.state === 'RESOLVED' &&
        limitRow.actuated === false &&
        limitActuated.length === 0 &&
        limitRow.localOutcome?.cause === 'denied' &&
        limitRow.localOutcome?.status === 409,
      `row ${JSON.stringify(sanitizeDeep(limitRow))} actuated writes ${limitActuated.length}`,
    );
    const limitLifecycleAfter = rb.rows(limitTicket.ticket);
    checks.check('DENY-LIMIT-NO-LIFECYCLE', canon(limitLifecycleAfter) === canon(limitBeforeDenial));
    evidence.limit.denied = {
      status: limitDenied.status,
      reason: limitDenied.json?.reason ?? null,
      occurrence: limitDenied.body.occurrence_id,
      wire: wireOf(limitDenied),
      alert: { shown: limitResult.shown, text: limitResult.text, expected: LIMIT_ALERT },
      occurrenceRow: sanitizeDeep(limitRow),
      rowsObserved: sanitizeDeep(limitRowsSeen),
      writes: limitWrites,
      actuatedWriteCount: limitActuated.length,
      storageIdle: limitIdle,
      lifecycleBefore: limitBeforeDenial,
      lifecycleAfter: limitLifecycleAfter,
    };
  } finally {
    await context.close();
  }
}
