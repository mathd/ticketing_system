import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  createPageCoordinator,
  type DecisionWriter,
  type FinalDecision,
  type ProposedDecision,
  type TransitionResult,
} from './occurrence-coordinator'
import type { OwnerLock } from './occurrence-lock'
import { captured } from './test/scans'

// Coordinator unit tier. The writer below records calls and returns scripted
// transition results; it decides nothing about row state. Row-state enforcement
// is in the ST suites, over the production store.

const lock = (hold: () => Promise<void> = async () => undefined): OwnerLock => ({
  holdForPageLifetime: hold,
  runIfOwnerAbsent: async () => ({ ran: false }),
})
const coordinator = (hold?: () => Promise<void>) =>
  createPageCoordinator({ owner: { ownerId: 'owner-a' }, lock: lock(hold) })
const queue = (occurrenceId: string, decidedAt = '2026-10-18T09:00:01.000Z'): ProposedDecision =>
  ({ kind: 'queue', occurrenceId, outcome: { family: 'queue', cause: 'transport_failure' }, decidedAt })
const cancel = (occurrenceId: string): ProposedDecision =>
  ({ kind: 'terminal', occurrenceId, outcome: { family: 'terminal', cause: 'cancelled', requestMayHaveBeenSent: false }, decidedAt: '2026-10-18T09:00:02.000Z' })

function writer(results: Array<TransitionResult | Error | Promise<TransitionResult>>) {
  const calls: FinalDecision[] = []
  const recorder: DecisionWriter = {
    generation: 1,
    async persistDecision(decision) {
      calls.push(decision)
      const next = results.shift()
      if (next instanceof Error) throw next
      return next ?? { status: 'changed' }
    },
    async enrichDenial() {
      return { status: 'changed' }
    },
  }
  return { recorder, calls }
}

function minted(page: ReturnType<typeof coordinator>, overrides = {}) {
  const scan = captured(overrides)
  page.register(scan)
  page.mintStarted(scan.occurrenceId).committed()
  return scan
}

afterEach(() => vi.restoreAllMocks())

/** vi.spyOn over a prototype method chosen at run time. */
const spyOn = vi.spyOn as unknown as (object: object, method: string) => { mockImplementation(fn: (...args: unknown[]) => unknown): unknown }

describe('latch', () => {
  it('keeps the first final decision and returns it to every later proposal', () => {
    const page = coordinator()
    const scan = minted(page)
    const first = queue(scan.occurrenceId)
    const canonical = page.latch(first)
    expect(canonical).toEqual(first)
    expect(page.latch(cancel(scan.occurrenceId))).toBe(canonical)
    expect(page.latch(queue(scan.occurrenceId, '2026-10-18T09:00:09.000Z'))).toBe(canonical)
    expect(page.latched(scan.occurrenceId)).toBe(canonical)
  })

  it('makes no storage call on the way to a queue latch (TKT-558 D24, store half)', () => {
    const page = coordinator()
    const scan = minted(page)
    const calls: string[] = []
    for (const [target, method] of [
      [IDBFactory.prototype, 'open'],
      [IDBFactory.prototype, 'deleteDatabase'],
      [IDBDatabase.prototype, 'transaction'],
      [IDBObjectStore.prototype, 'get'],
      [IDBObjectStore.prototype, 'put'],
      [IDBObjectStore.prototype, 'add'],
    ] as const) {
      const real = (target as unknown as Record<string, (...args: unknown[]) => unknown>)[method]
      spyOn(target, method).mockImplementation(function (this: unknown, ...args: unknown[]) {
        calls.push(method)
        return real.apply(this, args)
      })
    }
    const decision = queue(scan.occurrenceId)
    expect(page.latch(decision)).toEqual(decision)
    calls.push('latched')
    expect(calls).toEqual(['latched'])
  })
})

describe('persistence', () => {
  it('joins one running attempt per occurrence and retries the same target after a failure', async () => {
    const page = coordinator()
    const scan = minted(page)
    let finish!: (result: TransitionResult) => void
    const pending = new Promise<TransitionResult>((resolve) => { finish = resolve })
    const { recorder, calls } = writer([pending, new Error('abort'), { status: 'changed' }])
    page.attach(recorder)
    const decision = page.latch(queue(scan.occurrenceId))
    const first = page.persist(scan.occurrenceId)
    const joined = page.persist(scan.occurrenceId)
    expect(joined).toBe(first)
    finish({ status: 'changed' })
    expect((await first).durable).toBe('committed')
    expect(calls).toEqual([decision])
    expect(page.pendingTargets()).toEqual([])
  })

  it('keeps a failed target with a repair warning and retries it at the next repair point', async () => {
    const page = coordinator()
    const scan = minted(page)
    const { recorder, calls } = writer([new Error('abort'), { status: 'changed' }])
    page.attach(recorder)
    const decision = page.latch(queue(scan.occurrenceId))
    expect((await page.persist(scan.occurrenceId)).durable).toBe('failed')
    expect(page.repairWarnings()).toEqual([{ occurrenceId: scan.occurrenceId, kind: 'write_needs_repair' }])
    expect(page.pendingTargets()).toEqual([scan.occurrenceId])
    expect((await page.repairAll()).map((report) => report.durable)).toEqual(['committed'])
    expect(calls).toEqual([decision, decision])
    expect(page.repairWarnings()).toEqual([])
  })

  it.each(['missing', 'foreign'] as const)('reports a %s row as unrepairable and never retries it in a loop', async (status) => {
    const page = coordinator()
    const scan = minted(page)
    const { recorder, calls } = writer([{ status }])
    page.attach(recorder)
    page.latch(queue(scan.occurrenceId))
    expect((await page.persist(scan.occurrenceId)).durable).toBe('unrepairable')
    expect(await page.repairAll()).toEqual([])
    expect(calls).toHaveLength(1)
    expect(page.repairWarnings()).toEqual([{ occurrenceId: scan.occurrenceId, kind: 'cannot_repair' }])
  })

  it('defers a target while no writer is attached', async () => {
    const page = coordinator()
    const scan = minted(page)
    page.latch(queue(scan.occurrenceId))
    expect((await page.persist(scan.occurrenceId)).durable).toBe('deferred')
    const { recorder } = writer([{ status: 'changed' }])
    page.attach(recorder)
    expect((await page.repairAll()).map((report) => report.durable)).toEqual(['committed'])
  })

  it('writes nothing for a decision whose mint aborted, and drops it from the repair set', async () => {
    const page = coordinator()
    const scan = captured({ type: 'refusal' })
    page.register(scan)
    const observer = page.mintStarted(scan.occurrenceId)
    const { recorder, calls } = writer([])
    page.attach(recorder)
    page.latch({ kind: 'queue', occurrenceId: scan.occurrenceId, outcome: { family: 'queue', cause: 'local_refusal' }, decidedAt: '2026-10-18T09:00:01.000Z' })
    const persisting = page.persist(scan.occurrenceId)
    observer.aborted()
    expect((await persisting).durable).toBe('no_row')
    expect(calls).toEqual([])
    expect(page.pendingTargets()).toEqual([])
  })
})

describe('page owner lock', () => {
  it('acquires the lock once for the page and allows a retry after a failed acquisition', async () => {
    const hold = vi.fn<() => Promise<void>>().mockRejectedValueOnce(new Error('no locks')).mockResolvedValue(undefined)
    const page = coordinator(hold)
    await expect(page.acquireLock()).rejects.toThrow('no locks')
    expect(page.lockHeld()).toBe(false)
    await page.acquireLock()
    await page.acquireLock()
    expect(page.lockHeld()).toBe(true)
    expect(hold).toHaveBeenCalledTimes(2)
  })
})
