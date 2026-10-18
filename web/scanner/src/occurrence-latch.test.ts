import { forceCloseDatabase } from 'fake-indexeddb'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { FinalDecision, ProposedDecision } from './occurrence-coordinator'
import type { QueueOutcome, TerminalOutcome } from './occurrence-model'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Hold, type Instrument } from './test/idb-instrument'
import { bounded, captured, createTestPage, destroyAllPages, eventually, type TestPage } from './test/page-fixture'
import { rawGet } from './test/raw-db'

// I3-10, ST half (TKT-558 D26 as corrected by D28). The first final decision for
// an occurrence stays fixed for the page. Each case latches a write target, then
// lets one later producer act while that write is held or after it failed, and
// checks the latch identity and the exact durable row. The AP half (published
// result region) belongs to WU3. The case manifest and its exclusions are data.

type Family =
  | 'U01' | 'denial-known' | 'denial-unknown' | 'O01' | 'O02' | 'local-refusal'
  | 'U02' | 'U03' | 'X01' | 'G01' | 'S02' | 'C01'
type Producer = 'exception' | 'pairing' | 'unexpected-close' | 'versionchange' | 'deletion' | 'repair' | 'renewal'
type WriteMode = 'held' | 'throw' | 'abort'

const FAMILIES: Family[] = ['U01', 'denial-known', 'denial-unknown', 'O01', 'O02', 'local-refusal', 'U02', 'U03', 'X01', 'G01', 'S02', 'C01']
const PRODUCERS: Producer[] = ['exception', 'pairing', 'unexpected-close', 'versionchange', 'deletion', 'repair', 'renewal']
const MODES: WriteMode[] = ['held', 'throw', 'abort']

/** Reasoned exclusions, ADR-085 §6: none of these permits a fabricated cell or kill. */
export const LATCH_EXCLUSIONS = [
  'ACT x held/failed target write: a committed ACT has no new target write (controls below).',
  'G01 on an existing ACT, missing, foreign or ineligible row: no authorized terminal write; G01 here is on own PEND.',
  'S02 after a confirmed mint abort: no target row; S02 here is a mint that committed after the decision latched.',
  'local refusal x late response/body: a refusal sends no live request.',
  'U01 x late body: U01 invokes no body.',
  'O01 x late response/body: the one native promise already settled.',
  'O02 and U03 x late body: they ignore the body.',
  'U02, G01, ACT and denial-known x late body: the relevant body already settled.',
  'X01 and S02 x late response/body: no request is outstanding in these profiles.',
  'Completion of a competing renewal, repair or delete inside a held occurrence readwrite transaction: IndexedDB serializes it after the hold, so the held cases release before they wait.',
]

const T0 = 1_000_000
const FIRST = '2026-10-18T09:00:01.000Z'
const LATER = '2026-10-18T09:00:09.000Z'
const OCC_RW_STRICT = { stores: [STORE.occurrences], mode: 'readwrite', durability: 'strict' } as const

function proposal(family: Family, occurrenceId: string): ProposedDecision {
  const terminal = (outcome: TerminalOutcome): ProposedDecision => ({ kind: 'terminal', occurrenceId, outcome, decidedAt: FIRST })
  const queue = (outcome: QueueOutcome): ProposedDecision => ({ kind: 'queue', occurrenceId, outcome, decidedAt: FIRST })
  switch (family) {
    case 'U01': return terminal({ family: 'terminal', cause: 'auth_refused' })
    case 'denial-known': return terminal({ family: 'terminal', cause: 'denied', status: 409, latchId: `latch-${occurrenceId}`, reason: 'refunded' })
    case 'denial-unknown': return terminal({ family: 'terminal', cause: 'denied', status: 422, latchId: `latch-${occurrenceId}`, reason: null })
    case 'O01': return queue({ family: 'queue', cause: 'transport_failure' })
    case 'O02': return queue({ family: 'queue', cause: 'server_unavailable', status: 502 })
    case 'local-refusal': return queue({ family: 'queue', cause: 'local_refusal' })
    case 'U02': return terminal({ family: 'terminal', cause: 'unconfirmed_accepted_body' })
    case 'U03': return terminal({ family: 'terminal', cause: 'unconfirmed_server_error', status: 500 })
    case 'X01': return terminal({ family: 'terminal', cause: 'local_failure', requestMayHaveBeenSent: true })
    case 'G01': return terminal({ family: 'terminal', cause: 'mark_failure' })
    case 'S02': return terminal({ family: 'terminal', cause: 'mint_unconfirmed' })
    case 'C01': return terminal({ family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: true })
  }
}

const laterException = (occurrenceId: string): ProposedDecision =>
  ({ kind: 'terminal', occurrenceId, outcome: { family: 'terminal', cause: 'local_failure', requestMayHaveBeenSent: true }, decidedAt: LATER })
const laterCancellation = (occurrenceId: string): ProposedDecision =>
  ({ kind: 'terminal', occurrenceId, outcome: { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: true }, decidedAt: LATER })
const lateDenial = (occurrenceId: string): ProposedDecision =>
  ({ kind: 'terminal', occurrenceId, outcome: { family: 'terminal', cause: 'denied', status: 409, latchId: 'late-latch', reason: 'exchanged' }, decidedAt: LATER })

let dbName: string
let instrument: Instrument | undefined
beforeEach(() => {
  dbName = `latch-${crypto.randomUUID()}`
})
afterEach(() => {
  instrument?.restore()
  instrument = undefined
  destroyAllPages()
})

const forceClose = (db: IDBDatabase) => forceCloseDatabase(db as never)

function versionChangeThenAbortUpgrade(): Promise<void> {
  return new Promise((resolve) => {
    const request = indexedDB.open(dbName, 4)
    request.onupgradeneeded = () => request.transaction!.abort()
    request.onerror = () => resolve()
    request.onsuccess = () => { request.result.close(); resolve() }
  })
}

function deleteDatabase(): Promise<void> {
  return new Promise((resolve, reject) => {
    const request = indexedDB.deleteDatabase(dbName)
    request.onsuccess = () => resolve()
    request.onerror = () => reject(request.error)
  })
}

function pendingRow(scan: ReturnType<typeof captured>) {
  const base = {
    occurrenceId: scan.occurrenceId,
    qrPayload: scan.qrPayload,
    occurredAt: scan.occurredAt,
    createdAt: scan.createdAt,
    state: 'PENDING',
    actuated: false,
    ownerId: 'owner-a',
    leaseExpiresAt: T0 + 30_000,
    recordFormat: 1,
  }
  return scan.type === 'refusal' ? { ...base, localDecision: 'revocation_refused' } : { ...base, direction: 'entry' }
}
const expectedRow = (scan: ReturnType<typeof captured>, decision: ProposedDecision, reason?: string) => ({
  ...pendingRow(scan),
  state: decision.kind === 'queue' ? 'QUEUED' : 'RESOLVED',
  localOutcome: reason === undefined ? decision.outcome : { ...decision.outcome, reason },
  decidedAt: decision.decidedAt,
})

type Setup = { a: TestPage; scan: ReturnType<typeof captured>; latch: FinalDecision; persisting: Promise<unknown>; hold?: Hold }

/** Mints the occurrence, latches the family's decision and starts its first write in the given mode. */
async function latchedWrite(family: Family, mode: WriteMode): Promise<Setup> {
  const a = createTestPage({ dbName, ownerId: 'owner-a', now: () => T0, renewalMs: 3_600_000 })
  await a.ready()
  instrument = instrumentTransactions()
  const scan = captured({ type: family === 'local-refusal' ? 'refusal' : 'normal' })
  a.coordinator.register(scan)
  let mintHold: Hold | undefined
  let minting: Promise<unknown>
  if (family === 'S02') {
    // The decision latches while the mint is still unobserved; the mint then commits.
    mintHold = instrument.hold(OCC_RW_STRICT)
    minting = a.store().mintNormal(scan.occurrenceId)
    await bounded(mintHold.started, 'mint held')
  } else {
    minting = scan.type === 'refusal' ? a.store().mintRefusal(scan.occurrenceId) : a.store().mintNormal(scan.occurrenceId)
    await minting
  }
  const proposed = proposal(family, scan.occurrenceId)
  const latch = a.coordinator.latch(proposed)
  expect(latch).toBe(proposed)
  let hold: Hold | undefined
  if (mode === 'held') hold = instrument.hold(OCC_RW_STRICT)
  else instrument.inject(mode, OCC_RW_STRICT)
  const persisting = a.coordinator.persist(scan.occurrenceId)
  mintHold?.release()
  await minting
  if (hold) await bounded(hold.started, 'final write held')
  else expect((await persisting as { durable: string }).durable).toBe('failed')
  return { a, scan, latch, persisting, hold }
}

/** Runs the later producer. A held write is released before anything waits for a competing write. */
async function produce(setup: Setup, producer: Producer, late?: (id: string) => ProposedDecision): Promise<{ deleted: boolean }> {
  const { a, scan, latch, persisting, hold } = setup
  const id = scan.occurrenceId
  const release = async () => {
    if (!hold) return
    hold.release()
    await bounded(persisting, 'held write')
  }
  switch (producer) {
    case 'exception':
    case 'pairing': {
      const offered = late ? late(id) : producer === 'exception' ? laterException(id) : laterCancellation(id)
      expect(a.coordinator.latch(offered)).toBe(latch)
      await release()
      await a.coordinator.repairAll()
      return { deleted: false }
    }
    case 'unexpected-close':
      forceClose(a.store().db)
      await release()
      await a.waitFor('failed')
      a.storage.retry()
      await a.waitFor('ready')
      return { deleted: false }
    case 'versionchange': {
      const changing = versionChangeThenAbortUpgrade()
      await eventually(() => a.storage.state().status !== 'ready', 'draining')
      await release()
      await bounded(changing, 'version change')
      await a.waitFor('closed')
      a.storage.retry()
      await a.waitFor('ready')
      return { deleted: false }
    }
    case 'deletion': {
      const deleting = deleteDatabase()
      await eventually(() => a.storage.state().status !== 'ready', 'draining')
      await release()
      await bounded(deleting, 'deletion')
      await a.waitFor('closed')
      a.storage.retry()
      await a.waitFor('ready')
      return { deleted: true }
    }
    case 'repair': {
      const repairing = a.coordinator.repairAll()
      await release()
      await bounded(repairing, 'repair')
      await a.coordinator.repairAll()
      return { deleted: false }
    }
    case 'renewal': {
      const renewing = a.store().renewOwnedPending()
      await release()
      await bounded(renewing, 'renewal')
      await a.coordinator.repairAll()
      return { deleted: false }
    }
  }
}

async function check(setup: Setup, deleted: boolean, reason?: string) {
  const { a, scan, latch } = setup
  expect(a.coordinator.latched(scan.occurrenceId)).toBe(latch)
  const row = await rawGet(dbName, STORE.occurrences, scan.occurrenceId)
  if (deleted) {
    // Deletion removes the row; nothing recreates it, and the decision stays.
    expect(row).toBeUndefined()
    return
  }
  expect(row).toStrictEqual(expectedRow(scan, latch as ProposedDecision, reason))
  const listed = (await a.store().queued()).entries.map((entry) => entry.key)
  expect(listed).toEqual(latch.kind === 'queue' ? [scan.occurrenceId] : [])
}

const matrix = FAMILIES.flatMap((family) =>
  PRODUCERS.flatMap((producer) => MODES.map((mode) => [family, producer, mode] as const)))

describe('latch matrix: write target x later producer x write mode', () => {
  it.each(matrix)('%s, %s, %s write', async (family, producer, mode) => {
    const setup = await latchedWrite(family, mode)
    const { deleted } = await produce(setup, producer)
    await check(setup, deleted)
  })
})

describe('late response and body cells', () => {
  it.each(MODES)('C01 x late denial response, %s write', async (mode) => {
    const setup = await latchedWrite('C01', mode)
    await produce(setup, 'pairing', lateDenial)
    await check(setup, false)
  })

  it.each(MODES)('denial-unknown x its outstanding body, %s write: guarded enrichment only', async (mode) => {
    const setup = await latchedWrite('denial-unknown', mode)
    expect(setup.a.coordinator.captureDenialReason(setup.scan.occurrenceId, 'refunded')).toBe(true)
    expect(setup.a.coordinator.latch(lateDenial(setup.scan.occurrenceId))).toBe(setup.latch)
    setup.hold?.release()
    await setup.a.coordinator.repairAll()
    await setup.a.coordinator.repairAll()
    await check(setup, false, 'refunded')
  })
})

describe('committed ACT controls', () => {
  async function committedAct() {
    const a = createTestPage({ dbName, ownerId: 'owner-a', now: () => T0, renewalMs: 3_600_000 })
    await a.ready()
    const scan = captured()
    a.coordinator.register(scan)
    await a.store().mintNormal(scan.occurrenceId)
    a.coordinator.beginMarking(scan.occurrenceId)
    expect(await a.store().reserveActuation(scan.occurrenceId)).toEqual({ reserved: true })
    const latch = a.coordinator.latched(scan.occurrenceId)!
    const row = await rawGet(dbName, STORE.occurrences, scan.occurrenceId)
    instrument = instrumentTransactions()
    return { a, scan, latch, row }
  }

  it.each(['exception', 'pairing', 'unexpected-close', 'versionchange', 'deletion', 'repair', 'renewal'] as const)('ACT x %s: no new final write', async (producer) => {
    const { a, scan, latch, row } = await committedAct()
    const setup: Setup = { a, scan, latch, persisting: Promise.resolve() }
    if (producer === 'exception') {
      // A separately failing storage operation is diagnostic only.
      instrument!.inject('abort', { stores: [STORE.recoveryNotices], mode: 'readwrite' })
      await expect(a.store().acknowledgeNotice('none')).resolves.toBe('missing')
    }
    const { deleted } = await produce(setup, producer)
    expect(a.coordinator.latched(scan.occurrenceId)).toBe(latch)
    expect(a.coordinator.repairWarnings()).toEqual([])
    const finalWrites = instrument!.calls.filter((call) => call.mode === 'readwrite' && call.options?.durability === 'strict'
      && call.storeNames.length === 1 && call.storeNames[0] === STORE.occurrences)
    expect(finalWrites).toEqual([])
    if (!deleted) expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(row)
  })
})

describe('in-flight mark under a cancellation latch (I3-10-CS)', () => {
  async function markInFlight() {
    const a = createTestPage({ dbName, ownerId: 'owner-a', now: () => T0, renewalMs: 3_600_000 })
    await a.ready()
    const scan = captured()
    a.coordinator.register(scan)
    await a.store().mintNormal(scan.occurrenceId)
    a.coordinator.beginMarking(scan.occurrenceId)
    instrument = instrumentTransactions()
    const hold = instrument.hold(OCC_RW_STRICT)
    const marking = a.store().reserveActuation(scan.occurrenceId)
    const call = await bounded(hold.started, 'mark held')
    await eventually(() => call.events.includes('write-success'), 'mark request success')
    const cancellation = laterCancellation(scan.occurrenceId)
    expect(a.coordinator.latch(cancellation)).toBe(cancellation)
    return { a, scan, hold, marking, cancellation }
  }

  const finalWrites = () => instrument!.calls.slice(1).filter((call) => call.mode === 'readwrite' && call.options?.durability === 'strict'
    && call.storeNames.length === 1 && call.storeNames[0] === STORE.occurrences)

  it('keeps ACT with no terminal write, no repair warning and no opening when the mark commits', async () => {
    const { a, scan, hold, marking } = await markInFlight()
    const persisting = a.coordinator.persist(scan.occurrenceId)
    hold.release()
    expect(await bounded(marking, 'mark')).toEqual({ reserved: true })
    expect((await bounded(persisting, 'persist')).durable).toBe('settled_as_actuation')
    expect(finalWrites()).toEqual([])
    expect(a.coordinator.repairWarnings()).toEqual([])
    expect(a.coordinator.latched(scan.occurrenceId)?.kind).toBe('terminal')
    expect((await rawGet(dbName, STORE.occurrences, scan.occurrenceId) as Record<string, unknown>).state).toBe('ACTUATED')
  })

  it('writes TERM(C01) when the mark aborts', async () => {
    const { a, scan, hold, marking, cancellation } = await markInFlight()
    const persisting = a.coordinator.persist(scan.occurrenceId)
    hold.release('abort')
    await expect(bounded(marking, 'mark')).rejects.toThrow()
    expect((await bounded(persisting, 'persist')).durable).toBe('committed')
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(expectedRow(scan, cancellation))
  })

  it('keeps TERM(C01) as a protected repair target when the mark aborts and its write fails', async () => {
    const { a, scan, hold, marking, cancellation } = await markInFlight()
    instrument!.inject('abort', OCC_RW_STRICT)
    const persisting = a.coordinator.persist(scan.occurrenceId)
    hold.release('abort')
    await expect(bounded(marking, 'mark')).rejects.toThrow()
    expect((await bounded(persisting, 'persist')).durable).toBe('failed')
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(pendingRow(scan))
    await a.coordinator.repairAll()
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(expectedRow(scan, cancellation))
  })
})

describe('closing connection and deferred targets', () => {
  it('treats a transaction-start failure on a closing connection as a failed attempt, without touching readiness', async () => {
    const a = createTestPage({ dbName, ownerId: 'owner-a', now: () => T0, renewalMs: 3_600_000 })
    const old = await a.ready()
    const scan = captured()
    a.coordinator.register(scan)
    await old.mintNormal(scan.occurrenceId)
    forceClose(old.db)
    await a.waitFor('failed')
    a.storage.retry()
    await a.waitFor('ready')
    const latch = a.coordinator.latch(proposal('O02', scan.occurrenceId))
    // The old store's connection is closed: transaction creation throws InvalidStateError.
    expect((await a.coordinator.persist(scan.occurrenceId, old)).durable).toBe('failed')
    expect(a.storage.state()).toMatchObject({ status: 'ready' })
    await a.coordinator.repairAll()
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(expectedRow(scan, latch as ProposedDecision))
  })

  it('defers a target latched while no connection is attached, and keeps the enrichment for later', async () => {
    const a = createTestPage({ dbName, ownerId: 'owner-a', now: () => T0, renewalMs: 3_600_000 })
    await a.ready()
    const scan = captured()
    a.coordinator.register(scan)
    await a.store().mintNormal(scan.occurrenceId)
    const changing = versionChangeThenAbortUpgrade()
    await a.waitFor('closed')
    await changing
    const latch = a.coordinator.latch(proposal('denial-unknown', scan.occurrenceId))
    expect((await a.coordinator.persist(scan.occurrenceId)).durable).toBe('deferred')
    expect(a.coordinator.captureDenialReason(scan.occurrenceId, 'refunded')).toBe(true)
    expect(a.coordinator.view(scan.occurrenceId)?.enrichment).toBe('pending')
    a.storage.retry()
    await a.waitFor('ready')
    expect(await rawGet(dbName, STORE.occurrences, scan.occurrenceId)).toStrictEqual(expectedRow(scan, latch as ProposedDecision, 'refunded'))
  })
})

it('enumerates the reachable cases', () => {
  // 12 families x 7 producers x 3 write modes, plus 6 late response/body cells, 7 ACT
  // controls, 3 in-flight-mark cases and 2 connection cases.
  expect(matrix).toHaveLength(252)
  expect(LATCH_EXCLUSIONS).toHaveLength(10)
})
