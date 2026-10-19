// Test-only control of IndexedDB transactions created by production code.
//
// The spy wraps IDBDatabase.prototype.transaction. It records every call with
// its arguments and its complete or abort event, and it can apply one action to
// the next transaction that matches: a synchronous creation throw, an abort
// after the first write request succeeds, a QuotaExceededError thrown by the
// first write, or a hold that keeps the transaction open until release. The
// spy never supplies a row state; production code reads and writes real rows.

import { vi } from 'vitest'

export type TransactionCall = {
  storeNames: string[]
  mode: IDBTransactionMode
  options: IDBTransactionOptions | undefined
  events: string[]
  tx?: IDBTransaction
  action?: InjectionKind
}

export type InjectionKind = 'throw' | 'abort' | 'quota' | 'hold'
export type TransactionMatch = {
  stores?: readonly string[]
  mode?: IDBTransactionMode
  durability?: IDBTransactionDurability | 'none'
  /** Extra predicate, checked last. */
  when?: (call: TransactionCall) => boolean
}

export type Hold = {
  /** Resolves when the matching transaction exists and its hold is running. */
  started: Promise<TransactionCall>
  /** Ends the hold. commit lets the transaction finish; abort aborts it. */
  release(mode?: 'commit' | 'abort'): void
}

type Rule = { match: TransactionMatch; kind: InjectionKind; error?: unknown; hold?: HoldControl; onStore?: string }
type HoldControl = { released: boolean; abort: boolean; onStart: (call: TransactionCall) => void; watchdogMs: number }

const sameStores = (a: readonly string[], b: readonly string[]) =>
  a.length === b.length && [...a].sort().every((name, index) => name === [...b].sort()[index])

function matches(call: TransactionCall, match: TransactionMatch): boolean {
  if (match.stores && !sameStores(call.storeNames, match.stores)) return false
  if (match.mode && call.mode !== match.mode) return false
  if (match.durability) {
    const durability = call.options?.durability ?? 'none'
    if (durability !== match.durability) return false
  }
  return match.when ? match.when(call) : true
}

export function quotaError() {
  return new DOMException('injected quota failure', 'QuotaExceededError')
}

export function instrumentTransactions() {
  const original = IDBDatabase.prototype.transaction
  const originalClose = IDBDatabase.prototype.close
  const calls: TransactionCall[] = []
  const rules: Rule[] = []
  /** One ordered log of transaction events and connection closes, across all connections. */
  const log: string[] = []
  const closeSpy = vi.spyOn(IDBDatabase.prototype, 'close').mockImplementation(function (this: IDBDatabase) {
    log.push(`close v${this.version}`)
    return originalClose.call(this)
  })

  const wrapWrites = (
    tx: IDBTransaction,
    onWrite: (request: IDBRequest, store: IDBObjectStore) => void,
    throwInstead?: () => unknown,
    onStore?: string,
  ) => {
    const objectStore = tx.objectStore.bind(tx)
    let fired = false
    tx.objectStore = (name: string) => {
      const os = objectStore(name)
      if (onStore !== undefined && name !== onStore) return os
      for (const method of ['put', 'add', 'delete'] as const) {
        const real = (os[method] as (...args: unknown[]) => IDBRequest).bind(os)
        ;(os as unknown as Record<string, unknown>)[method] = (...args: unknown[]) => {
          if (!fired && throwInstead) {
            fired = true
            throw throwInstead()
          }
          const request = real(...args)
          if (!fired) {
            fired = true
            onWrite(request, os)
          }
          return request
        }
      }
      return os
    }
  }

  const spy = vi.spyOn(IDBDatabase.prototype, 'transaction').mockImplementation(function (
    this: IDBDatabase,
    storeNames: string | Iterable<string>,
    mode?: IDBTransactionMode,
    options?: IDBTransactionOptions,
  ) {
    const call: TransactionCall = {
      storeNames: typeof storeNames === 'string' ? [storeNames] : [...storeNames],
      mode: mode ?? 'readonly',
      options,
      events: [],
    }
    calls.push(call)
    const id = calls.length - 1
    log.push(`create#${id} ${call.mode} ${call.storeNames.join('+')}`)
    const index = rules.findIndex((rule) => matches(call, rule.match))
    const rule = index >= 0 ? rules.splice(index, 1)[0] : undefined
    call.action = rule?.kind
    if (rule?.kind === 'throw') {
      call.events.push('throw')
      throw rule.error ?? new DOMException('injected creation failure', 'InvalidStateError')
    }
    const tx = options === undefined
      ? original.call(this, storeNames, mode)
      : original.call(this, storeNames, mode, options)
    call.tx = tx
    tx.addEventListener('complete', () => { call.events.push('complete'); log.push(`complete#${id}`) })
    tx.addEventListener('abort', () => { call.events.push('abort'); log.push(`abort#${id}`) })
    if (rule?.kind === 'abort') {
      wrapWrites(tx, (request) => {
        request.addEventListener('success', () => {
          call.events.push('write-success')
          tx.abort()
        })
      }, undefined, rule.onStore)
    }
    if (rule?.kind === 'quota') wrapWrites(tx, () => undefined, () => rule.error ?? quotaError(), rule.onStore)
    if (rule?.kind === 'hold' && rule.hold) {
      const control = rule.hold
      wrapWrites(tx, (request) => {
        request.addEventListener('success', () => call.events.push('write-success'))
      })
      const pumpStore = call.storeNames[0]
      const startedAt = Date.now()
      const pump = () => {
        if (control.released) {
          if (control.abort) tx.abort()
          return
        }
        if (Date.now() - startedAt > control.watchdogMs) {
          call.events.push('hold-watchdog')
          tx.abort()
          return
        }
        const request = tx.objectStore(pumpStore).count()
        request.onsuccess = pump
      }
      pump()
      control.onStart(call)
    }
    return tx
  })

  return {
    calls,
    /** Applies one action to the next matching transaction. */
    inject(kind: Exclude<InjectionKind, 'hold'>, match: TransactionMatch, options: { error?: unknown; onStore?: string } = {}) {
      rules.push({ match, kind, error: options.error, onStore: options.onStore })
    },
    /** Keeps the next matching transaction open until release. A watchdog aborts it and fails the test. */
    hold(match: TransactionMatch, watchdogMs = 3_000): Hold {
      let onStart!: (call: TransactionCall) => void
      const started = new Promise<TransactionCall>((resolve) => { onStart = resolve })
      const control: HoldControl = { released: false, abort: false, onStart, watchdogMs }
      rules.push({ match, kind: 'hold', hold: control })
      return {
        started,
        release: (mode = 'commit') => {
          control.abort = mode === 'abort'
          control.released = true
        },
      }
    },
    log,
    pendingRules: () => rules.length,
    restore: () => {
      spy.mockRestore()
      closeSpy.mockRestore()
    },
  }
}

export type Instrument = ReturnType<typeof instrumentTransactions>
