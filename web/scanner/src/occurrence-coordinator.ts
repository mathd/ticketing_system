// The page coordinator (ADR-085 §2.1-§2.3, I1-I3).
//
// One coordinator lives for the whole JavaScript page. It owns the page owner
// id, the page owner lock, every captured occurrence, the final decision latch
// and the unsettled final writes. A database connection borrows it; closing or
// replacing a connection never replaces it. React cleanup never touches it.
//
// The first final decision for an occurrence stays fixed for the life of the
// page (the latch rule, TKT-558 D26). A failed write, a later response, a
// connection event, a repair or a renewal cannot select another decision. They
// can only retry the same target.

import { createBrowserOwnerLock, type OwnerLock } from './occurrence-lock'
import type { QueueOutcome, ScanDirection, TerminalOutcome } from './occurrence-model'
import { createOccurrenceOwner, type OccurrenceOwner } from './occurrence-owner'

export type CapturedOccurrence = Readonly<{
  occurrenceId: string
  qrPayload: string
  occurredAt: string
  createdAt: string
} & ({ type: 'normal'; direction: ScanDirection } | { type: 'refusal'; direction?: never })>

export type QueueDecision = Readonly<{ kind: 'queue'; occurrenceId: string; outcome: Readonly<QueueOutcome>; decidedAt: string }>
export type TerminalDecision = Readonly<{ kind: 'terminal'; occurrenceId: string; outcome: Readonly<TerminalOutcome>; decidedAt: string }>
/** A committed own-row actuation reservation. Only the coordinator makes this decision. */
export type ActuationDecision = Readonly<{ kind: 'actuation'; occurrenceId: string; decidedAt: string }>
export type FinalDecision = QueueDecision | TerminalDecision | ActuationDecision
export type ProposedDecision = QueueDecision | TerminalDecision

/** The result of one guarded store transition that committed or found nothing to change. */
export type TransitionResult =
  | { status: 'changed' }
  | { status: 'alreadySame' }
  | { status: 'missing' }
  | { status: 'foreign' }
  | { status: 'ineligible'; why: string }

/** The store side of a final write. It throws when the transaction does not commit. */
export interface DecisionWriter {
  readonly generation: number
  persistDecision(decision: FinalDecision): Promise<TransitionResult>
  enrichDenial(decision: TerminalDecision, reason: string): Promise<TransitionResult>
}

export type DurableStatus =
  | 'none'
  /** The latched decision committed, or the row already held it. */
  | 'committed'
  /** A mark committed while another decision was latched. ACT stays; no other write. */
  | 'settled_as_actuation'
  /** The mint never committed, so no row exists to write. */
  | 'no_row'
  /** The last attempt threw or aborted. The target stays protected for a later attempt. */
  | 'failed'
  /** No compatible connection was attached. The target stays protected. */
  | 'deferred'
  /** The row is missing, foreign or in another state. Never recreated or overwritten. */
  | 'unrepairable'

/** unstarted: registered, mint transaction not created yet. pending: created, not settled. */
export type MintEvidence = 'unstarted' | 'pending' | 'committed' | 'aborted'
export type MarkEvidence = 'pending' | 'reserved' | 'not_reserved'

export type RepairWarning = { occurrenceId: string; kind: 'write_needs_repair' | 'cannot_repair' }

export type OccurrenceView = {
  captured: CapturedOccurrence
  mint: MintEvidence
  phase: 'WAITING' | 'MARKING'
  mark: MarkEvidence | 'none'
  final: FinalDecision | undefined
  durable: DurableStatus
  enrichment: 'none' | 'pending' | 'committed' | 'refused'
}

export type PersistReport = { occurrenceId: string; durable: DurableStatus; enrichment: OccurrenceView['enrichment'] }

/** Reports the settlement of one mint or mark transaction to the coordinator. */
export type TransactionObserver = { committed(): void; aborted(): void }

type Entry = {
  captured: CapturedOccurrence
  mint: MintEvidence
  mintSettled: Promise<void>
  settleMint: () => void
  phase: 'WAITING' | 'MARKING'
  mark: MarkEvidence | 'none'
  markSettled?: Promise<void>
  final?: FinalDecision
  durable: DurableStatus
  repair?: Promise<PersistReport>
  denialReason?: string
  enrichment: OccurrenceView['enrichment']
}

export interface PageCoordinator {
  readonly ownerId: string
  /** The previous owner of this tab, known only after a genuine page reload. */
  readonly recoverOwnerId: string | undefined
  readonly lock: OwnerLock
  /** Acquires the page owner lock once for the page lifetime. A failure can be retried. */
  acquireLock(): Promise<void>
  lockHeld(): boolean

  /** Registers a captured occurrence before its mint transaction starts. */
  register(captured: CapturedOccurrence): void
  view(occurrenceId: string): OccurrenceView | undefined
  /** The store calls this when it creates the mint transaction. */
  mintStarted(occurrenceId: string): TransactionObserver
  /** The scan controller calls this after a valid accepted body. */
  beginMarking(occurrenceId: string): boolean
  /** True only for a committed mint in MARKING with no final decision and no settled mark. */
  canReserve(occurrenceId: string): boolean
  /** The store calls this when it creates the actuation transaction. */
  markStarted(occurrenceId: string, decidedAt: string): { reserved(existingDecidedAt?: string): void; notReserved(): void; aborted(): void }

  /** Latches the first final decision. A later proposal returns the existing latch unchanged. */
  latch(proposal: ProposedDecision): FinalDecision
  latched(occurrenceId: string): FinalDecision | undefined
  /** Keeps a valid denial reason for the one guarded enrichment after the denial commits. */
  captureDenialReason(occurrenceId: string, reason: string): boolean

  /** The writer that new final writes use. The readiness controller sets it. */
  attach(writer: DecisionWriter): void
  detach(writer: DecisionWriter): void
  /** Writes the latched decision for one occurrence. Joins a running attempt for the same id. */
  persist(occurrenceId: string, writer?: DecisionWriter): Promise<PersistReport>
  /** Attempts every unsettled target and pending enrichment once. Resolves after every attempt settles. */
  repairAll(writer?: DecisionWriter): Promise<PersistReport[]>
  /** Settles in-flight mint and mark evidence, then attempts every target on the given old connection. */
  drain(writer: DecisionWriter): Promise<PersistReport[]>
  /** Occurrence ids whose latched decision has no durable result yet. */
  pendingTargets(): string[]
  repairWarnings(): RepairWarning[]
}

export type PageCoordinatorOptions = {
  owner: OccurrenceOwner
  lock: OwnerLock
}

export function createPageCoordinator({ owner, lock }: PageCoordinatorOptions): PageCoordinator {
  const entries = new Map<string, Entry>()
  // Confirmed failed mints release their captured credentials. Keep only the id
  // and any immutable decision so later callbacks cannot start another attempt.
  const abortedMints = new Map<string, FinalDecision | undefined>()
  // Ids whose final write or enrichment has not settled. Settled entries leave
  // this set; their latch stays in `entries` for the life of the page.
  const targets = new Set<string>()
  let writer: DecisionWriter | undefined
  let lockPromise: Promise<void> | undefined
  let held = false

  const entryFor = (occurrenceId: string): Entry => {
    const entry = entries.get(occurrenceId)
    if (!entry) throw new Error(`occurrence ${occurrenceId} is not registered on this page`)
    return entry
  }

  const view = (entry: Entry): OccurrenceView => ({
    captured: entry.captured,
    mint: entry.mint,
    phase: entry.phase,
    mark: entry.mark,
    final: entry.final,
    durable: entry.durable,
    enrichment: entry.enrichment,
  })

  const report = (entry: Entry): PersistReport => ({
    occurrenceId: entry.captured.occurrenceId,
    durable: entry.durable,
    enrichment: entry.enrichment,
  })

  const settled = (entry: Entry) =>
    (entry.durable === 'committed' || entry.durable === 'settled_as_actuation'
      || entry.durable === 'no_row' || entry.durable === 'unrepairable')
    && entry.enrichment !== 'pending'

  const attempt = async (entry: Entry, using: DecisionWriter | undefined): Promise<PersistReport> => {
    const final = entry.final
    if (!final) return report(entry)
    // A mint or mark in flight settles first. Its transaction decides whether a row exists
    // and whether ACT committed (ADR-085 §2.3).
    if (entry.mint === 'pending') await entry.mintSettled
    if (entry.mark === 'pending') await entry.markSettled

    if (entry.mint === 'unstarted') {
      // The scan controller has not created the mint transaction yet. Nothing can be written.
      return report(entry)
    }
    if (entry.mint === 'aborted') {
      entry.durable = 'no_row'
    } else if (final.kind === 'actuation') {
      entry.durable = 'committed'
    } else if (entry.mark === 'reserved') {
      // The mark committed although another decision latched first. ACT stays, with no
      // terminal write, no repair warning and no opening (LATCH RULE, C01 in flight).
      entry.durable = 'settled_as_actuation'
    } else if (entry.durable !== 'committed') {
      if (!using) {
        entry.durable = 'deferred'
      } else {
        try {
          const result = await using.persistDecision(final)
          entry.durable = result.status === 'changed' || result.status === 'alreadySame' ? 'committed' : 'unrepairable'
        } catch {
          entry.durable = 'failed'
        }
      }
    }

    // One guarded reason-only enrichment, only after the denial committed (TKT-558 D11, D22).
    if (entry.enrichment === 'pending' && entry.durable === 'committed' && final.kind === 'terminal' && entry.denialReason) {
      if (using) {
        try {
          const result = await using.enrichDenial(final, entry.denialReason)
          entry.enrichment = result.status === 'changed' || result.status === 'alreadySame' ? 'committed' : 'refused'
        } catch {
          // The attempt spends nothing. The reason stays for a later compatible connection.
        }
      }
    }
    if (settled(entry)) targets.delete(entry.captured.occurrenceId)
    return report(entry)
  }

  const persist = (occurrenceId: string, using?: DecisionWriter): Promise<PersistReport> => {
    if (abortedMints.has(occurrenceId)) return Promise.resolve({ occurrenceId, durable: 'no_row', enrichment: 'none' })
    const entry = entryFor(occurrenceId)
    if (!entry.final) return Promise.resolve(report(entry))
    // At most one attempt per occurrence. A second request joins it.
    if (entry.repair) return entry.repair
    if (settled(entry)) return Promise.resolve(report(entry))
    const running = attempt(entry, using ?? writer).finally(() => {
      if (entry.repair === running) entry.repair = undefined
    })
    entry.repair = running
    return running
  }

  const repairAll = async (using?: DecisionWriter): Promise<PersistReport[]> => {
    const ids = [...targets]
    const results = await Promise.allSettled(ids.map((id) => persist(id, using)))
    return results.flatMap((result) => (result.status === 'fulfilled' ? [result.value] : []))
  }

  return {
    ownerId: owner.ownerId,
    recoverOwnerId: owner.recoverOwnerId,
    lock,

    acquireLock() {
      if (!lockPromise) {
        const attemptLock = lock.holdForPageLifetime(owner.ownerId).then(() => {
          held = true
        })
        lockPromise = attemptLock
        attemptLock.catch(() => {
          if (lockPromise === attemptLock) lockPromise = undefined
        })
      }
      return lockPromise
    },
    lockHeld: () => held,

    register(captured) {
      if (entries.has(captured.occurrenceId) || abortedMints.has(captured.occurrenceId)) throw new Error(`occurrence ${captured.occurrenceId} is already registered`)
      let settleMint!: () => void
      const mintSettled = new Promise<void>((resolve) => { settleMint = resolve })
      entries.set(captured.occurrenceId, {
        captured: Object.freeze({ ...captured }),
        mint: 'unstarted',
        mintSettled,
        settleMint,
        phase: 'WAITING',
        mark: 'none',
        durable: 'none',
        enrichment: 'none',
      })
    },

    view(occurrenceId) {
      const entry = entries.get(occurrenceId)
      return entry ? view(entry) : undefined
    },

    mintStarted(occurrenceId) {
      const entry = entryFor(occurrenceId)
      if (entry.mint !== 'unstarted') throw new Error(`occurrence ${occurrenceId} already started its mint`)
      entry.mint = 'pending'
      const settle = (evidence: 'committed' | 'aborted') => {
        if (entry.mint !== 'pending') return
        entry.mint = evidence
        entry.settleMint()
        if (evidence === 'aborted') {
          abortedMints.set(occurrenceId, entry.final)
          entries.delete(occurrenceId)
          targets.delete(occurrenceId)
        }
      }
      return { committed: () => settle('committed'), aborted: () => settle('aborted') }
    },

    beginMarking(occurrenceId) {
      const entry = entries.get(occurrenceId)
      if (!entry || entry.mint !== 'committed' || entry.final || entry.phase === 'MARKING') return false
      entry.phase = 'MARKING'
      return true
    },

    canReserve(occurrenceId) {
      const entry = entries.get(occurrenceId)
      return !!entry && entry.mint === 'committed' && entry.phase === 'MARKING' && !entry.final
        && (entry.mark === 'none' || entry.mark === 'pending')
    },

    markStarted(occurrenceId, decidedAt) {
      const entry = entryFor(occurrenceId)
      if (entry.mark !== 'none') throw new Error(`occurrence ${occurrenceId} already started its mark`)
      entry.mark = 'pending'
      let settleMark!: () => void
      entry.markSettled = new Promise<void>((resolve) => { settleMark = resolve })
      const settle = (evidence: 'reserved' | 'not_reserved', confirmedAt = decidedAt) => {
        if (entry.mark !== 'pending') return
        entry.mark = evidence
        if (evidence === 'reserved') {
          if (!entry.final) {
            entry.final = Object.freeze({ kind: 'actuation', occurrenceId, decidedAt: confirmedAt })
            entry.durable = 'committed'
          } else if (entry.final.kind !== 'actuation') {
            entry.durable = 'settled_as_actuation'
            targets.delete(occurrenceId)
          }
        }
        settleMark()
      }
      return {
        reserved: (existingDecidedAt) => settle('reserved', existingDecidedAt),
        notReserved: () => settle('not_reserved'),
        aborted: () => settle('not_reserved'),
      }
    },

    latch(proposal) {
      const canonical = () => Object.freeze({ ...proposal, outcome: Object.freeze({ ...proposal.outcome }) }) as ProposedDecision
      if (abortedMints.has(proposal.occurrenceId)) {
        const final = abortedMints.get(proposal.occurrenceId) ?? canonical()
        abortedMints.set(proposal.occurrenceId, final)
        return final
      }
      const entry = entryFor(proposal.occurrenceId)
      if (entry.final) return entry.final
      entry.final = canonical()
      targets.add(proposal.occurrenceId)
      return entry.final
    },

    latched: (occurrenceId) => entries.get(occurrenceId)?.final ?? abortedMints.get(occurrenceId),

    captureDenialReason(occurrenceId, reason) {
      const entry = entries.get(occurrenceId)
      if (!entry) return false
      const final = entry.final
      if (!final || final.kind !== 'terminal' || final.outcome.cause !== 'denied' || final.outcome.reason !== null) return false
      if (entry.enrichment !== 'none' || !reason) return false
      entry.denialReason = reason
      entry.enrichment = 'pending'
      targets.add(occurrenceId)
      return true
    },

    attach(next) {
      writer = next
    },
    detach(previous) {
      if (writer === previous) writer = undefined
    },

    persist,
    repairAll,

    async drain(old) {
      const inFlight: Promise<void>[] = []
      for (const entry of entries.values()) {
        if (entry.mint === 'pending') inFlight.push(entry.mintSettled)
        if (entry.mark === 'pending' && entry.markSettled) inFlight.push(entry.markSettled)
      }
      await Promise.all(inFlight)
      // A running attempt may use the connection that is closing. Let it settle, then
      // attempt every remaining target on this still-open connection before it closes.
      await Promise.allSettled([...entries.values()].flatMap((entry) => (entry.repair ? [entry.repair] : [])))
      return repairAll(old)
    },

    pendingTargets: () => [...targets],

    repairWarnings() {
      const warnings: RepairWarning[] = []
      for (const entry of entries.values()) {
        const occurrenceId = entry.captured.occurrenceId
        if (entry.durable === 'failed' || entry.durable === 'deferred') warnings.push({ occurrenceId, kind: 'write_needs_repair' })
        if (entry.durable === 'unrepairable') warnings.push({ occurrenceId, kind: 'cannot_repair' })
      }
      return warnings
    },
  }
}

/** The one production page coordinator, created at module initialization. */
export const pageCoordinator: PageCoordinator = createPageCoordinator({
  owner: createOccurrenceOwner(),
  lock: createBrowserOwnerLock(),
})
