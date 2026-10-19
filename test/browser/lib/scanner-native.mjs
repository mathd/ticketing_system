import { createServer } from 'node:http';
import { assert, pair, submit, read, scanPath, reconcilePath, installProbe, arm, matched, release } from './scanner-controls.mjs';

export async function nativeControls({ browser, BASE, token, check, fixture, facts, quarantine }) {
  for (const status of [409,422]) {
    const context=await browser.newContext({baseURL:BASE}); await installProbe(context);
    const page=await context.newPage(); let responseStream;
    const server=createServer((request,response)=>{
      if(request.method==='OPTIONS'){response.writeHead(204,{'Access-Control-Allow-Origin':BASE,'Access-Control-Allow-Headers':'content-type,x-scanner-token','Access-Control-Allow-Methods':'POST'});response.end();return;}
      request.resume();request.on('end',()=>{response.writeHead(status,{'Content-Type':'application/json','Access-Control-Allow-Origin':BASE});response.flushHeaders();response.write('{');responseStream=response;});
    });
    await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
    try{
      await pair(page,token);const control=fixture(),denied=fixture();
      await page.route(scanPath,route=>route.fulfill({status:503,body:'pre-decision'}));await submit(page,control.credential);await page.getByRole('heading',{name:'Offline decision'}).waitFor();
      await page.waitForFunction(()=>window.__tkt558IdbProbe.events.some(e=>e.state==='QUEUED'&&e.event==='complete'));
      await page.unroute(scanPath);
      const sent=page.waitForRequest(request=>request.method()==='POST'&&scanPath(request.url()));
      await page.route(scanPath,route=>route.continue({url:`http://127.0.0.1:${server.address().port}/stalled-body`}));
      await submit(page,denied.credential);const id=(await sent).postDataJSON().occurrence_id;
      await page.waitForFunction(id=>window.__tkt558IdbProbe.events.some(e=>e.id===id&&e.state==='RESOLVED'&&e.event==='complete'),id);
      const {latchId,...outcome}=(await read(page)).find(row=>row.occurrenceId===id).localOutcome;
      assert.match(latchId,/^[0-9a-f-]{36}$/);assert.deepEqual(outcome,{family:'terminal',cause:'denied',reason:null,status});
      assert.equal(await page.getByText('Checking...', {exact:true}).count(),1);
      await page.reload();responseStream?.end('"decision":"rejected","reason":"refunded"}');await page.unroute(scanPath);
      await page.getByRole('button',{name:'Sync queued scans'}).waitFor();
      const request=page.waitForRequest(request=>request.method()==='POST'&&reconcilePath(request.url()));
      await page.getByRole('button',{name:'Sync queued scans'}).click();
      assert.equal((await request).postDataJSON().occurrences.some(row=>row.occurrence_id===id),false);
      await page.getByText('Synced 1 offline scan(s).',{exact:true}).waitFor();
      assert.deepEqual(facts(denied.ticket),[]);assert.equal(quarantine(denied.ticket),0);
      check(`B6 native ${status} headers commit unknown TERM while body stalls; reload sends only its real queued control`,true);
    }finally{responseStream?.destroy();await context.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
  }

  for(const status of [409,422])for(const failure of ['hold','throw','abort','body interruption']){
    const context=await browser.newContext({baseURL:BASE});await installProbe(context);
    const page=await context.newPage();let stream;
    const server=createServer((request,response)=>{
      if(request.method==='OPTIONS'){response.writeHead(204,{'Access-Control-Allow-Origin':BASE,'Access-Control-Allow-Headers':'content-type,x-scanner-token','Access-Control-Allow-Methods':'POST'});response.end();return;}
      request.resume();request.on('end',()=>{response.writeHead(status,{'Content-Type':'application/json','Access-Control-Allow-Origin':BASE});response.flushHeaders();response.write('{');stream=response;});
    });
    await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
    try{
      await pair(page,token);const ticket=fixture();
      await page.route(scanPath,route=>route.continue({url:`http://127.0.0.1:${server.address().port}/enrichment`}));
      const sent=page.waitForRequest(request=>request.method()==='POST'&&scanPath(request.url()));
      await submit(page,ticket.credential);const id=(await sent).postDataJSON().occurrence_id;
      await page.waitForFunction(id=>window.__tkt558IdbProbe.events.some(e=>e.id===id&&e.state==='RESOLVED'&&e.event==='complete'),id);
      const original=(await read(page))[0];assert.equal(original.localOutcome.reason,null);assert.equal(original.localOutcome.status,status);
      if(failure==='body interruption')stream.destroy();
      else{
        await arm(page,{id,state:'RESOLVED',reason:'refunded',action:failure,once:false,name:'enrichment'});
        stream.end('"decision":"rejected","reason":"refunded"}');
      }
      await page.getByRole('heading',{name:'Rejected',exact:true}).waitFor();
      if(failure==='body interruption')assert.deepEqual((await read(page))[0],original);
      else{
        await matched(page,{id,state:'RESOLVED'});
        if(failure==='hold'){
          await page.waitForFunction(()=>!!window.__tkt558IdbProbe.holds.enrichment);
          await release(page,'enrichment');
        }else{
          await page.waitForFunction(id=>window.__tkt558IdbProbe.events.some(e=>e.id===id&&e.event==='matched'&&e.action!=='hold'),id);
          assert.deepEqual((await read(page))[0],original);
          await page.evaluate(()=>{window.__tkt558IdbProbe.rules=[];});
          await page.evaluate(()=>window.dispatchEvent(new Event('online')));
        }
        await page.waitForFunction(id=>window.__tkt558IdbProbe.events.filter(e=>e.id===id&&e.event==='complete'&&e.state==='RESOLVED').length>=2,id);
        assert.deepEqual((await read(page))[0],{...original,localOutcome:{...original.localOutcome,reason:'refunded'}});
      }
      assert.equal(await page.getByRole('heading',{name:'Offline decision'}).count(),0);assert.deepEqual(facts(ticket.ticket),[]);assert.equal(quarantine(ticket.ticket),0);
      check(`B6 ${status} ${failure} retains status and denial latch with reason-only enrichment`,true);
    }finally{stream?.destroy();await context.close();server.closeAllConnections();await new Promise(resolve=>server.close(resolve));}
  }
  for(const failure of ['invalid header','non-network TypeError','manual redirect']){
    const context=await browser.newContext({baseURL:BASE});const page=await context.newPage();
    let followed=0;
    try{
      await pair(page,token);const ticket=fixture();const requests=[];
      page.on('request',request=>{if(request.method()==='POST'&&scanPath(request.url()))requests.push(request);});
      if(failure==='invalid header')await page.evaluate(()=>{localStorage.setItem('scanner.device-token','bad\nheader');window.dispatchEvent(new StorageEvent('storage',{key:'scanner.device-token',newValue:'bad\nheader'}));});
      if(failure==='non-network TypeError')await page.evaluate(()=>{const native=fetch;window.fetch=(input,init)=>{if(input instanceof Request&&new URL(input.url).pathname==='/api/access/scans')throw new TypeError('local setup error');return native(input,init);};});
      if(failure==='manual redirect'){
        await page.route('**/redirect-counter',route=>{followed++;return route.fulfill({status:200,body:'{}'});});
        await page.route(scanPath,route=>route.fulfill({status:307,headers:{location:BASE+'/redirect-counter'},body:''}));
      }
      await submit(page,ticket.credential);await page.getByRole('heading',{name:'Operator required',exact:true}).waitFor();
      assert.equal(await page.getByRole('heading',{name:'Offline decision'}).count(),0);
      const rows=await read(page);assert.equal(rows.length,1);assert.equal(rows[0].state,'RESOLVED');
      assert.equal(rows[0].localOutcome.cause,failure==='manual redirect'?'unconfirmed_status':'local_failure');
      if(failure!=='manual redirect')assert.equal(requests.length,0);
      assert.equal(followed,0);assert.deepEqual(facts(ticket.ticket),[]);assert.equal(quarantine(ticket.ticket),0);
      check(`B6 ${failure} fails closed with no followed scan POST or offline permission`,true);
    }finally{await context.close();}
  }
}
