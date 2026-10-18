import { afterEach, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { forceCloseDatabase } from 'fake-indexeddb'
import App from './App'
import type { StorageController } from './occurrence-readiness'
import { createTestPage, destroyAllPages, type TestPage } from './test/page-fixture'
import { rawGet } from './test/raw-db'
import { STORE } from './occurrence-store'
import { instrumentTransactions, type Hold } from './test/idb-instrument'
const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async load => ({ ...await load<typeof import('./occurrence-readiness')>(), get pageStorage() { return runtime.storage! } }))
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
const families = ['U01','denial-known','denial-unknown','O01','O02','local-refusal','U02','U03','X01','G01','S02','C01'] as const
const producers = ['exception','pairing','unexpected-close','versionchange','deletion','repair','renewal'] as const
const modes = ['held','throw','abort'] as const
const matrix = families.flatMap(family => producers.flatMap(producer => modes.map(mode => ({ family,producer,mode }))))
// The ST case manifest lists the protocol exclusions. These AP cells execute the
// controller and preserve its whole result region. A retry or renewal does not
// propose a new outcome and is a control. Competing writes complete after a hold.
const reset = () => { localStorage.setItem('scanner.device-token','token-b'); window.dispatchEvent(new StorageEvent('storage',{key:'scanner.device-token',newValue:'token-b'})) }
const region = () => screen.queryByRole('alert',{name:'Scan result'})?.textContent ?? screen.queryByRole('status',{name:'Scan result'})?.textContent ?? null
const tickReady = (page:TestPage) => waitFor(() => expect(page.storage.state().status).toBe('ready'))
async function changedConnection(page:TestPage, operation:'unexpected-close'|'versionchange'|'deletion', release:()=>void) {
  if(operation==='unexpected-close') {
    forceCloseDatabase(page.store().db as never); release()
    await waitFor(()=>expect(page.storage.state().status).toBe('failed'))
  } else {
    const done=new Promise<void>((resolve,reject)=>{
      const request=operation==='deletion'?indexedDB.deleteDatabase(page.store().db.name):indexedDB.open(page.store().db.name,4)
      request.onupgradeneeded=()=>request.transaction!.abort()
      request.onerror=()=>operation==='versionchange'?resolve():reject(request.error)
      request.onsuccess=()=>{if('result' in request&&request.result instanceof IDBDatabase)request.result.close();resolve()}
    })
    await waitFor(()=>expect(page.storage.state().status).not.toBe('ready'));release();await done
    await waitFor(()=>expect(page.storage.state().status).toBe('closed'))
  }
  await act(async()=>{page.storage.retry()});await tickReady(page)
}
it.each(matrix)('AP I3-10 $family / $producer / $mode retains its first latch and published region',async({family,producer,mode})=>{
  const dbName=`app-latch-${crypto.randomUUID()}`
  const page=createTestPage({dbName,now:()=>1000,renewalMs:producer==='renewal'?5:3600000})
  runtime.storage=page.storage;await page.ready();localStorage.setItem('scanner.device-token','token-a')
  const instrument=instrumentTransactions();let hold:Hold|undefined,id='',initial:Record<string,unknown>|undefined,armed=true,failures=0
  const put=IDBObjectStore.prototype.put
  vi.spyOn(IDBObjectStore.prototype,'put').mockImplementation(function(this:IDBObjectStore,value:unknown,...keys:[key?:IDBValidKey]){
    const row=value as {occurrenceId?:string;state?:string}
    const target=armed&&row.occurrenceId===id&&(row.state==='QUEUED'||row.state==='RESOLVED')&&mode!=='held'
    if(target&&mode==='throw'){failures++;throw new DOMException('injected final write','QuotaExceededError')}
    const request=Reflect.apply(put,this,[value,...keys])
    if(target){request.addEventListener('success',()=>{failures++;this.transaction.abort()})}
    return request
  })
  const store=page.store()
  const mint=family==='local-refusal'?store.mintRefusal.bind(store):store.mintNormal.bind(store)
  vi.spyOn(store,family==='local-refusal'?'mintRefusal':'mintNormal').mockImplementation(async occurrenceId=>{
    const row=await mint(occurrenceId);id=occurrenceId;initial={...row}
    if(mode==='held')hold=instrument.hold({stores:[STORE.occurrences],mode:'readwrite',durability:'strict'})
    if(family==='S02')throw new Error('mint completion unavailable to caller')
    return row
  })
  if(family==='G01')vi.spyOn(store,'reserveActuation').mockRejectedValue(new DOMException('mark creation failed','InvalidStateError'))
  let finishReply:((response:Response)=>void)|undefined
  const renewal=vi.spyOn(store,'renewOwnedPending')
  vi.stubGlobal('fetch',vi.fn(async(input:Request|string)=>{
    if(typeof input==='string')return input.includes('voided-tickets')?new Response(JSON.stringify({ticket_ids:[],next_cursor:null})):new Response('{}',{status:503})
    if(family==='C01')return new Promise<Response>(resolve=>{finishReply=resolve})
    if(family==='O01')throw new TypeError('Failed to fetch')
    if(family==='X01')throw new TypeError('local failure')
    const status=family==='U01'?401:family==='denial-known'?409:family==='denial-unknown'?422:family==='O02'?503:family==='U03'?500:200
    const body=family==='denial-known'?{decision:'rejected',reason:'refunded'}:family==='G01'?{decision:'accepted',scanned_at:'2026-10-18T12:34:56.789Z'}:{}
    return new Response(JSON.stringify(body),{status})
  }))
  let payload='credential'
  if(family==='local-refusal'){
    const ticketId='10000000-0000-4000-8000-000000000001';await store.mergeRevoked([ticketId])
    payload=`e30.${btoa(JSON.stringify({tid:ticketId})).replace(/=/g,'')}.sig`
  }
  try{
    render(<App/>);fireEvent.change(screen.getByLabelText('Ticket credential'),{target:{value:payload}})
    fireEvent.submit(screen.getByRole('form',{name:'Ticket scan'}))
    await waitFor(()=>expect(id).not.toBe(''))
    if(family==='C01'){await waitFor(()=>expect(finishReply).toBeDefined());await act(async()=>reset())}
    await waitFor(()=>expect(page.coordinator.latched(id)).toBeDefined())
    const first=page.coordinator.latched(id)!
    if(first.kind==='actuation')throw new Error('expected a non-actuation decision')
    if(mode==='held'){const call=await hold!.started;await waitFor(()=>expect(call.events).toContain('write-success'))}
    else await waitFor(()=>expect(failures).toBeGreaterThan(0))
    if(family!=='C01')await waitFor(()=>expect(region()).not.toBeNull())
    const published=region()
    if(family==='C01'){finishReply!(new Response(JSON.stringify({decision:'rejected',reason:'refunded'}),{status:409}));await act(async()=>{await Promise.resolve()});expect(page.coordinator.latched(id)).toBe(first)}
    const release=()=>hold?.release()
    if(producer==='pairing')await act(async()=>reset())
    else if(producer==='unexpected-close'||producer==='versionchange'||producer==='deletion')await changedConnection(page,producer,release)
    else if(producer==='renewal'){await waitFor(()=>expect(renewal).toHaveBeenCalled());release()}
    else {if(mode==='held')fireEvent(window,new Event('online'));else fireEvent.click(await screen.findByRole('button',{name:'Retry storage'}));release()}
    release();await act(async()=>{await page.coordinator.repairAll()});armed=false
    await waitFor(async()=>{await act(async()=>{await page.coordinator.repairAll()});expect(page.coordinator.view(id)?.durable).toBe(producer==='deletion'&&mode!=='held'?'unrepairable':'committed')})
    expect(page.coordinator.latched(id)).toBe(first)
    expect(region()).toBe(producer==='pairing'?null:published)
    if(producer==='deletion')expect(await rawGet(dbName,STORE.occurrences,id)).toBeUndefined()
    else expect(await rawGet(dbName,STORE.occurrences,id)).toStrictEqual({...initial,state:first.kind==='queue'?'QUEUED':'RESOLVED',localOutcome:family==='denial-known'?{...first.outcome,reason:'refunded'}:first.outcome,decidedAt:first.decidedAt})
    finishReply?.(new Response(JSON.stringify({decision:'rejected',reason:'refunded'}),{status:409}))
    await act(async()=>{await Promise.resolve()})
    expect(page.coordinator.latched(id)).toBe(first)
  }finally{armed=false;hold?.release();finishReply?.(new Response('{}',{status:500}));instrument.restore()}
})

it.each(producers)('AP I3-10 committed ACT / %s performs no new target write',async producer=>{
  const dbName=`app-act-${crypto.randomUUID()}`
  const page=createTestPage({dbName,now:()=>1000,renewalMs:producer==='renewal'?5:3600000});runtime.storage=page.storage;await page.ready()
  localStorage.setItem('scanner.device-token','token-a')
  vi.stubGlobal('fetch',vi.fn(async(input:Request|string)=>new Response(JSON.stringify(typeof input==='string'?{ticket_ids:[],next_cursor:null}:{decision:'accepted',scanned_at:'2026-10-18T12:34:56.789Z'}))))
  render(<App/>);fireEvent.change(screen.getByLabelText('Ticket credential'),{target:{value:'credential'}});fireEvent.submit(screen.getByRole('form',{name:'Ticket scan'}))
  await screen.findByRole('heading',{name:'Accepted'})
  const [id]=page.coordinator.pendingTargets()
  expect(id).toBeUndefined()
  const db=page.store().db
  const rows=await new Promise<Record<string,unknown>[]>((resolve,reject)=>{const request=db.transaction(STORE.occurrences).objectStore(STORE.occurrences).getAll();request.onsuccess=()=>resolve(request.result);request.onerror=()=>reject(request.error)})
  const original=rows[0],occurrenceId=original.occurrenceId as string,first=page.coordinator.latched(occurrenceId),published=region()
  let writes=0
  const put=IDBObjectStore.prototype.put
  vi.spyOn(IDBObjectStore.prototype,'put').mockImplementation(function(this:IDBObjectStore,value:unknown,...keys:[key?:IDBValidKey]){if((value as {occurrenceId?:string}).occurrenceId===occurrenceId)writes++;return Reflect.apply(put,this,[value,...keys])})
  if(producer==='pairing')await act(async()=>reset())
  else if(producer==='unexpected-close'||producer==='versionchange'||producer==='deletion')await changedConnection(page,producer,()=>{})
  else if(producer==='renewal'){const renewal=vi.spyOn(page.store(),'renewOwnedPending');await waitFor(()=>expect(renewal).toHaveBeenCalled())}
  else {
    if(producer==='exception')vi.spyOn(page.store(),'readDisposalSnapshot').mockRejectedValue(new DOMException('diagnostic read failure','UnknownError'))
    fireEvent(window,new Event('online'));await act(async()=>{await page.coordinator.repairAll()})
  }
  expect(page.coordinator.latched(occurrenceId)).toBe(first)
  expect(writes).toBe(0)
  expect(region()).toBe(producer==='pairing'?null:published)
  expect(await rawGet(dbName,STORE.occurrences,occurrenceId)).toStrictEqual(producer==='deletion'?undefined:original)
})
