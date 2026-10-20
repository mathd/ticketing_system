import { useEffect, useState } from 'react'
import { prepareExport, summarizeDisposal } from './occurrence-export'
import type { StorageController } from './occurrence-readiness'
import type { DisposalSnapshot, ExportReceipt, NoticeView } from './occurrence-store'
import { STORAGE_UNAVAILABLE } from './scan-result'

const EXPORT_FAILED = 'Export was not confirmed. Held records remain unresolved. Request another export before acknowledgement.'
const ACK_FAILED = 'Acknowledgement failed. Held records remain unresolved.'
const equalKey = (a: IDBValidKey, b: IDBValidKey) => indexedDB.cmp(a, b) === 0
const includesKey = (keys: readonly IDBValidKey[], key: IDBValidKey) => keys.some((item) => equalKey(item, key))

/** All pages show the durable records, independent of their pairing state. */
export function OccurrenceRecords({ storage, refresh }: { storage: StorageController; refresh: number }) {
  const [snapshot, setSnapshot] = useState<DisposalSnapshot>({ held: [], legacyQueued: [] })
  const [notices, setNotices] = useState<NoticeView[]>([])
  const [selected, setSelected] = useState<IDBValidKey[]>([])
  const [receipt, setReceipt] = useState<ExportReceipt | null>(null)
  const [message, setMessage] = useState('')
  const [readFailed, setReadFailed] = useState(false)
  const [noticeFailures, setNoticeFailures] = useState<Set<string>>(new Set())
  const [busy, setBusy] = useState(false)
  const [revision, setRevision] = useState(0)
  const [state, setState] = useState(storage.state)
  useEffect(() => storage.subscribe(setState), [storage])
  useEffect(() => {
    let active = true
    const store = storage.readyStore()
    if (store) void Promise.all([store.readDisposalSnapshot(), store.readNotices()]).then(([next, nextNotices]) => {
      if (!active) return
      setReadFailed(false)
      setSnapshot(next)
      setNotices(nextNotices)
      setSelected((previous) => previous.filter((key) => next.held.some((entry) => !entry.disposal && equalKey(entry.key, key))))
    }).catch(() => { if (active) setReadFailed(true) })
    return () => { active = false }
  }, [storage, state, refresh, revision])
  const summary = summarizeDisposal(snapshot)
  const ready = state.status === 'ready'
  const exported = receipt?.held.map((entry) => entry.occurrenceId) ?? []
  const canAcknowledge = !!receipt && selected.length > 0 && selected.every((key) => includesKey(exported, key))
  const exportRecords = async () => {
    if (busy) return
    setBusy(true)
    setReceipt(null)
    let url: string | undefined
    let anchor: HTMLAnchorElement | undefined
    try {
      const store = storage.readyStore()
      if (!store) throw new Error('storage unavailable')
      const prepared = prepareExport(await store.readDisposalSnapshot(), {
        exportId: crypto.randomUUID(), exportedAt: new Date().toISOString(), databaseName: store.db.name, databaseVersion: store.db.version,
      })
      url = URL.createObjectURL(new Blob([prepared.contents], { type: 'application/json' }))
      anchor = document.createElement('a')
      anchor.href = url
      anchor.download = `scanner-records-${prepared.document.exportId}.json`
      document.body.append(anchor)
      anchor.click()
      await store.commitExportReceipt(prepared.receipt)
      setReceipt(prepared.receipt)
      setMessage('Export requested. The browser cannot confirm that the file was saved.')
    } catch {
      setMessage(EXPORT_FAILED)
    } finally {
      anchor?.remove()
      if (url) URL.revokeObjectURL(url)
      setBusy(false)
    }
  }
  const acknowledge = async () => {
    if (!canAcknowledge || busy || !receipt) return
    setBusy(true)
    try {
      const store = storage.readyStore()
      if (!store) throw new Error('storage unavailable')
      const result = await store.acknowledgeHeld(receipt.receiptId, selected)
      if (result.status !== 'acknowledged') throw new Error('acknowledgement refused')
      setSelected([])
      setMessage('Selected held records acknowledged. They remain stored and will not sync.')
      setRevision((value) => value + 1)
    } catch {
      setMessage(ACK_FAILED)
    } finally {
      setBusy(false)
    }
  }
  const acknowledgeNotice = async (id: string) => {
    try {
      const store = storage.readyStore()
      if (!store) throw new Error('storage unavailable')
      await store.acknowledgeNotice(id)
      setNoticeFailures((previous) => { const next = new Set(previous); next.delete(id); return next })
      setRevision((value) => value + 1)
    } catch {
      setNoticeFailures((previous) => new Set(previous).add(id))
    }
  }
  return <section aria-label="Local scanner records" className="local-records">
    {summary.unresolvedCount > 0 && <p role="alert">{summary.unresolvedCount} held record(s) need operator review. These records will not sync.</p>}
    {summary.legacyQueuedCount > 0 && <p>{summary.legacyQueuedCount} pre-upgrade queued record(s) are listed for review. Their original cause is unknown.</p>}
    {summary.exportAvailable && <>
      <p>The export can contain ticket credentials. Keep the file private.</p>
      <button type="button" className="scanner-export" disabled={!ready || busy} onClick={() => void exportRecords()}>Export local records</button>
    </>}
    {snapshot.held.some((entry) => !entry.disposal) && <form aria-label="Held record acknowledgement" onSubmit={(event) => { event.preventDefault(); void acknowledge() }}>
      <fieldset disabled={!ready || busy}>
        <legend>Held records</legend>
        {snapshot.held.filter((entry) => !entry.disposal).map((entry, index) => <label key={index}>
          <input type="checkbox" checked={includesKey(selected, entry.key)} onChange={(event) => {
            const checked = event.target.checked
            setSelected((previous) => checked ? [...previous, entry.key] : previous.filter((key) => !equalKey(key, entry.key)))
          }} /> Held record {String(entry.key)}
        </label>)}
        <button type="submit" disabled={!canAcknowledge}>Acknowledge selected held records</button>
      </fieldset>
    </form>}
    {readFailed && ready && <p role="alert">{STORAGE_UNAVAILABLE}</p>}
    {message && <p role="status">{message}</p>}
    {notices.map((notice) => <section key={notice.occurrenceId} aria-label={`Recovered scan ${notice.occurrenceId}`}>
      <h2>Recovered scan</h2>
      <p>Scan {notice.occurrenceId} at {notice.occurredAt ?? 'unknown'}. Type: {notice.type === 'normal' ? notice.direction : notice.type ?? 'unknown'}. Recovery cause: uncertain_recovery. Sync state: {notice.syncState}.</p>
      <p>The prior result and physical passage are unknown. Do not admit from this notice.</p>
      <form onSubmit={(event) => { event.preventDefault(); void acknowledgeNotice(notice.occurrenceId) }}>
        <button type="submit" disabled={!ready}>Acknowledge recovery notice</button>
      </form>
      {noticeFailures.has(notice.occurrenceId) && <p role="alert">Notice acknowledgement failed. The notice remains.</p>}
    </section>)}
  </section>
}
