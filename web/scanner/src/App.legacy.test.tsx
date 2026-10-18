import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import App from './App'
import { seedDatabase, V2_STORES, rawGetAll } from './test/raw-db'
import { createTestPage, destroyAllPages } from './test/page-fixture'
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
  render(<App/>);fireEvent.click(await screen.findByRole('button',{name:'Sync queued scans'}))
  await screen.findByText('Synced 8 offline scan(s).')
  expect(screen.getByText('Pre-upgrade record odd could not sync. It remains queued and listed in the export.')).toBeDefined()
  const normal=sent.flat().filter(row=>row.occurrence_id!=='odd').sort((a,b)=>String(a.occurrence_id).localeCompare(String(b.occurrence_id)))
  expect(normal.map(row=>Object.hasOwn(row,'event_type')?row.event_type:'absent')).toEqual(['absent','invalid_legacy_direction','invalid_legacy_direction','invalid_legacy_direction','invalid_legacy_direction','invalid_legacy_direction','entry','exit'])
  expect(sent.find(batch=>batch.some(row=>row.occurrence_id==='odd'))).toStrictEqual([{occurrence_id:'odd',qr_payload:{malformed:'credential'},occurred_at:odd.occurredAt}])
  expect(await rawGetAll(dbName,STORE.occurrences)).toEqual(expect.arrayContaining([odd,...rows.map(row=>({...row,state:'SYNCED',result:'recorded'}))]))
})
