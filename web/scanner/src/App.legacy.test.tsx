import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, screen } from '@testing-library/react'
import App from './App'
import { renderWithQueue } from './test/render-queued'
import { seedDatabase, V2_STORES, rawGetAll } from './test/raw-db'
import { createTestPage, destroyAllPages, mintOn } from './test/page-fixture'
import type { StorageController } from './occurrence-readiness'
import { STORE } from './occurrence-store'
const runtime = vi.hoisted(() => ({ storage: null as StorageController | null }))
vi.mock('./occurrence-readiness', async (load) => ({ ...await load<typeof import('./occurrence-readiness')>(), get pageStorage() { return runtime.storage! } }))
beforeEach(() => { localStorage.setItem('scanner.device-token','token') })
afterEach(() => { cleanup(); destroyAllPages(); localStorage.clear(); vi.restoreAllMocks(); vi.unstubAllGlobals() })
const directions = [{}, { direction: undefined }, { direction: null }, { direction: '' }, { direction: 0 }, { direction: false }, { direction: 'entry' }, { direction: 'exit' }]
it('sends literal legacy direction classes and isolates an odd M02 beside a valid control', async () => {
  const dbName = `legacy-app-${crypto.randomUUID()}`
  const rows = directions.map((direction,index) => ({ occurrenceId: `row-${index}`, state:'QUEUED',qrPayload:'credential',occurredAt:'2026-10-18T12:34:56.789Z',actuated:false,...direction }))
  const odd = { occurrenceId:'odd',state:'QUEUED',qrPayload:{ malformed:'credential' },occurredAt:'2026-10-18T12:34:56.123Z',actuated:false }
  await seedDatabase(dbName,2,V2_STORES,{ occurrences:[...rows,odd] })
  const page=createTestPage({dbName});runtime.storage=page.storage;await page.ready()
  const sent:Array<Array<Record<string,unknown>>>=[]
  vi.stubGlobal('fetch',vi.fn(async(input:string,init?:RequestInit)=>{
    if(input.includes('voided-tickets'))return new Response(JSON.stringify({ticket_ids:[],next_cursor:null}))
    const batch=JSON.parse(init!.body as string).occurrences;sent.push(batch)
    return batch.some((row:Record<string,unknown>)=>typeof row.qr_payload!=='string')
      ?new Response('{}',{status:422})
      :new Response(JSON.stringify({results:batch.map((row:Record<string,unknown>)=>({occurrence_id:row.occurrence_id,result:'recorded'}))}))
  }))
  await renderWithQueue(<App/>, page);fireEvent.click(await screen.findByRole('button',{name:'Sync queued scans'}))
  await screen.findByText('Synced 8 offline scan(s).')
  expect(screen.getByText('Pre-upgrade record odd could not sync. It remains queued and listed in the export.')).toBeDefined()
  const normal=sent.flat().filter(row=>row.occurrence_id!=='odd').sort((a,b)=>String(a.occurrence_id).localeCompare(String(b.occurrence_id)))
  expect(normal.map(row=>Object.hasOwn(row,'event_type')?row.event_type:'absent')).toEqual(['absent','invalid_legacy_direction','invalid_legacy_direction','invalid_legacy_direction','invalid_legacy_direction','invalid_legacy_direction','entry','exit'])
  expect(sent.find(batch=>batch.some(row=>row.occurrence_id==='odd'))).toStrictEqual([{occurrence_id:'odd',qr_payload:{malformed:'credential'},occurred_at:odd.occurredAt}])
  expect(await rawGetAll(dbName,STORE.occurrences)).toEqual(expect.arrayContaining([odd,...rows.map(row=>({...row,state:'SYNCED',result:'recorded'}))]))
})

it('keeps an isolated M02 warning when the next batch fails', async () => {
  const dbName = `legacy-warning-${crypto.randomUUID()}`
  const odd = { occurrenceId:'a-odd',state:'QUEUED',qrPayload:{ malformed:'credential' },occurredAt:'2026-10-18T12:34:56.123Z',actuated:false }
  const valid = { occurrenceId:'b-valid',state:'QUEUED',qrPayload:'credential',occurredAt:'2026-10-18T12:34:56.789Z',actuated:false }
  await seedDatabase(dbName,2,V2_STORES,{ occurrences:[odd,valid] })
  const page=createTestPage({dbName});runtime.storage=page.storage;await page.ready()
  const sent:string[][]=[]
  vi.stubGlobal('fetch',vi.fn(async(input:string,init?:RequestInit)=>{
    if(input.includes('voided-tickets'))return new Response(JSON.stringify({ticket_ids:[],next_cursor:null}))
    const batch=JSON.parse(init!.body as string).occurrences;sent.push(batch.map((row:{occurrence_id:string})=>row.occurrence_id))
    return new Response('{}',{status:batch[0].occurrence_id==='a-odd'?422:500})
  }))
  await renderWithQueue(<App/>, page);fireEvent.click(await screen.findByRole('button',{name:'Sync queued scans'}))
  await screen.findByText(/^Sync failed: The server answered/)
  expect(sent).toStrictEqual([['a-odd'],['b-valid']])
  expect(screen.getByText('Pre-upgrade record a-odd could not sync. It remains queued and listed in the export.')).toBeDefined()
  expect(await rawGetAll(dbName,STORE.occurrences)).toStrictEqual([odd,valid])
})

it('preserves mixed Entry, refusal and literal M02 Exit across transport failure, 401 and retry', async () => {
  const dbName = `mixed-retry-${crypto.randomUUID()}`
  const legacy = { occurrenceId:'legacy-exit',state:'QUEUED',qrPayload:'old-credential',occurredAt:'2026-10-18T12:34:56.123Z',actuated:false,direction:'exit' }
  await seedDatabase(dbName,2,V2_STORES,{occurrences:[legacy]})
  const page=createTestPage({dbName});runtime.storage=page.storage;await page.ready()
  for(const type of ['normal','refusal'] as const){
    const {scan}=await mintOn(page,{type,qrPayload:`${type}-credential`})
    page.coordinator.latch({kind:'queue',occurrenceId:scan.occurrenceId,outcome:{family:'queue',cause:type==='refusal'?'local_refusal':'transport_failure'},decidedAt:'2026-10-18T12:35:00.789Z'})
    await page.coordinator.persist(scan.occurrenceId)
  }
  const before=await rawGetAll(dbName,STORE.occurrences) as Record<string,unknown>[]
  const expected=before.map(row=>({occurrence_id:row.occurrenceId,qr_payload:row.qrPayload,occurred_at:row.occurredAt,...(row.localDecision?{local_decision:'revocation_refused'}:{event_type:row.direction})}))
  const sent:unknown[]=[];let response=0
  vi.stubGlobal('fetch',vi.fn(async(input:string,init?:RequestInit)=>{
    if(input.includes('voided-tickets'))return new Response(JSON.stringify({ticket_ids:[],next_cursor:null}))
    const rows=JSON.parse(init!.body as string).occurrences;sent.push(rows)
    response++
    if(response===1)throw new TypeError('Failed to fetch')
    if(response===2)return new Response('',{status:401})
    return new Response(JSON.stringify({results:rows.map((row:{occurrence_id:string})=>({occurrence_id:row.occurrence_id,result:'recorded'}))}))
  }))
  await renderWithQueue(<App/>, page);fireEvent.click(await screen.findByRole('button',{name:'Sync queued scans'}))
  await screen.findByText(/^Sync failed: No connection\./)
  expect(await rawGetAll(dbName,STORE.occurrences)).toStrictEqual(before)
  fireEvent.click(screen.getByRole('button',{name:'Sync queued scans'}));await screen.findByLabelText('Pairing token')
  expect(await rawGetAll(dbName,STORE.occurrences)).toStrictEqual(before)
  fireEvent.change(screen.getByLabelText('Pairing token'),{target:{value:'replacement-token'}})
  fireEvent.submit(screen.getByRole('form',{name:'Device pairing'}))
  fireEvent.click(await screen.findByRole('button',{name:'Sync queued scans'}));await screen.findByText('Synced 3 offline scan(s).')
  expect(sent).toStrictEqual([expected,expected,expected])
  expect(await rawGetAll(dbName,STORE.occurrences)).toStrictEqual(before.map(row=>({...row,state:'SYNCED',result:'recorded'})))
})
