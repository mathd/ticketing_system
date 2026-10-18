import { ORGANIZER, sql } from './support.mjs';
import { assert, pair, submit, read, scanPath, reconcilePath, installProbe, arm, matched, release } from './scanner-controls.mjs';

export async function recovery({ browser, BASE, token, check, fixture, facts, quarantine, PG, ACCESS_PORT, INTERNAL_TOKEN }) {
  for (const boundary of ['before send', 'after Access commit']) {
    const context = await browser.newContext({ baseURL: BASE }); await installProbe(context);
    const page = await context.newPage();
    let releaseResponse, receivedResponse;
    const upstreamReady = new Promise(resolve => { receivedResponse = resolve; });
    try {
      await pair(page, token);
      const ticket = fixture();
      if (boundary === 'before send') await page.evaluate(() => {
        const native = fetch;
        window.fetch = (input, init) => input instanceof Request && new URL(input.url).pathname === '/api/access/scans' ? new Promise(() => {}) : native(input,init);
      });
      else await page.route(scanPath, async route => {
        const response = await route.fetch();
        assert.equal(response.status(),200);
        await new Promise(resolve => { releaseResponse = resolve; receivedResponse(); });
        await route.fulfill({ response }).catch(() => {});
      });
      await submit(page,ticket.credential);
      await page.waitForFunction(() => window.__tkt558IdbProbe.events.some(e => e.state === 'PENDING' && e.event === 'complete'));
      const original = (await read(page))[0]; assert.equal(original.state,'PENDING');
      if (boundary === 'after Access commit') {
        let watchdog;
        try { await Promise.race([upstreamReady, new Promise((_, reject) => { watchdog = setTimeout(() => reject(new Error('B5 upstream response did not arrive')), 10000); })]); } finally { clearTimeout(watchdog); }
        assert.equal(facts(ticket.ticket).length,1);
      } else assert.deepEqual(facts(ticket.ticket),[]);
      await page.reload(); releaseResponse?.(); await page.unrouteAll({ behavior:'ignoreErrors' });
      await page.getByRole('region',{ name:`Recovered scan ${original.occurrenceId}` }).waitFor();
      const expected = `Recovered scanScan ${original.occurrenceId} at ${original.occurredAt}. Type: entry. Recovery cause: uncertain_recovery. Sync state: QUEUED.The prior result and physical passage are unknown. Do not admit from this notice.Acknowledge recovery notice`;
      assert.equal(await page.getByRole('region',{ name:`Recovered scan ${original.occurrenceId}` }).textContent(),expected);
      assert.equal((await read(page))[0].decidedAt,undefined);
      const b = await context.newPage(); await pair(b,token);
      const sent = b.waitForRequest(request => request.method()==='POST' && reconcilePath(request.url()));
      await b.getByRole('button',{ name:'Sync queued scans' }).click();
      assert.deepEqual((await sent).postDataJSON().occurrences,[{ occurrence_id:original.occurrenceId,qr_payload:ticket.credential,occurred_at:original.occurredAt,event_type:'entry' }]);
      await b.getByText('Synced 1 offline scan(s).',{exact:true}).waitFor();
      assert.equal(facts(ticket.ticket).length,1); assert.equal(quarantine(ticket.ticket),0);
      if (boundary==='before send') assert.deepEqual(facts(ticket.ticket),[{ occurrence_id:original.occurrenceId,event_type:'entry',occurred_at:original.occurredAt }]);
      const c = await context.newPage(); await pair(c,token);
      await c.getByText(new RegExp(`Scan ${original.occurrenceId}.*Sync state: SYNCED`)).waitFor();
      await page.getByRole('button',{ name:'Sync queued scans' }).click();
      await page.getByText(new RegExp(`Scan ${original.occurrenceId}.*Sync state: SYNCED`)).waitFor();
      await c.getByRole('button',{name:'Acknowledge recovery notice'}).click();
      await c.getByRole('heading',{name:'Recovered scan',exact:true}).waitFor({state:'hidden'});
      const retired = await read(c);
      for (let n=0;n<2;n++) { await c.reload(); await c.getByLabel('Ticket credential').waitFor(); assert.equal(await c.getByRole('heading',{name:'Recovered scan',exact:true}).count(),0); }
      assert.deepEqual(await read(c),retired);
      check(`B5 genuine reload ${boundary} retains exact Entry and a notice across three pages, retirement and acknowledgement`,true);
    } finally { releaseResponse?.(); await context.close(); }
  }

  // A mint that aborts before complete cannot leave a fact for another page.
  const context = await browser.newContext({baseURL:BASE}); await installProbe(context);
  const page = await context.newPage();
  try {
    await pair(page,token); const ticket=fixture();
    await arm(page,{state:'PENDING',action:'hold',once:true,name:'mint'});
    await submit(page,ticket.credential); await matched(page,{state:'PENDING'});
    await page.waitForFunction(() => !!window.__tkt558IdbProbe.holds.mint);
    await release(page,'mint',true);
    await page.getByText('Scanner could not confirm that this scan was saved. No request was sent. Do not admit from this result. Get an operator.').waitFor();
    await page.reload(); await page.getByLabel('Ticket credential').waitFor();
    assert.deepEqual(await read(page),[]); assert.deepEqual(facts(ticket.ticket),[]); assert.equal(quarantine(ticket.ticket),0);
    check('B5 aborted mint leaves no local or Access fact after reload',true);
  } finally {await context.close();}
  for (const kind of ['typed refusal', 'lost terminal write']) {
    const context=await browser.newContext({baseURL:BASE});await installProbe(context);
    const a=await context.newPage();await a.clock.install();
    try {
      const ticket=fixture();
      if(kind==='typed refusal'){
        const response=await fetch(`http://localhost:${ACCESS_PORT}/internal/orders/${ticket.order}/refunds`,{method:'POST',headers:{'Content-Type':'application/json','X-Internal-Token':INTERNAL_TOKEN},body:JSON.stringify({organizer_id:ORGANIZER,refund_id:crypto.randomUUID(),quantity:1})});
        assert.equal(response.status,200);
      }
      await pair(a,token);
      const baseline=facts(ticket.ticket), quarantined=quarantine(ticket.ticket);
      await arm(a,{state:kind==='typed refusal'?'QUEUED':'RESOLVED',action:'throw',once:false});
      if(kind==='lost terminal write')await a.route(scanPath,route=>route.fulfill({status:409,contentType:'application/json',body:JSON.stringify({decision:'rejected',reason:'refunded'})}));
      await submit(a,ticket.credential);await matched(a,{state:kind==='typed refusal'?'QUEUED':'RESOLVED'});
      await a.getByRole('heading',{name:kind==='typed refusal'?'Not valid for entry':'Rejected',exact:true}).waitFor();
      const [original]=await read(a);assert.equal(original.state,'PENDING');assert.equal(original.decidedAt,undefined);
      if(kind==='typed refusal'){assert.equal(original.localDecision,'revocation_refused');assert.equal(Object.hasOwn(original,'direction'),false);}
      else assert.equal(original.direction,'entry');
      const existing=await context.newPage();await pair(existing,token);
      await a.close();await existing.clock.fastForward(31000);
      const b=await context.newPage();await pair(b,token);
      const notice=b.getByRole('region',{name:`Recovered scan ${original.occurrenceId}`});await notice.waitFor();
      assert.equal(await notice.textContent(),`Recovered scanScan ${original.occurrenceId} at ${original.occurredAt}. Type: ${kind==='typed refusal'?'refusal':'entry'}. Recovery cause: uncertain_recovery. Sync state: QUEUED.The prior result and physical passage are unknown. Do not admit from this notice.Acknowledge recovery notice`);
      const [recovered]=await read(b);assert.deepEqual(recovered,{...original,state:'QUEUED',localOutcome:{family:'queue',cause:'uncertain_recovery'}});
      const sent=b.waitForRequest(request=>request.method()==='POST'&&reconcilePath(request.url()));
      await b.getByRole('button',{name:'Sync queued scans'}).click();
      assert.deepEqual((await sent).postDataJSON().occurrences,[{occurrence_id:original.occurrenceId,qr_payload:ticket.credential,occurred_at:original.occurredAt,...(kind==='typed refusal'?{local_decision:'revocation_refused'}:{event_type:'entry'})}]);
      await b.getByText('Synced 1 offline scan(s).',{exact:true}).waitFor();
      if(kind==='typed refusal'){
        assert.deepEqual(facts(ticket.ticket),baseline);assert.equal(quarantine(ticket.ticket),quarantined);
        assert.equal(sql(PG,'access',`SELECT count(*) FROM scanner_local_decisions WHERE occurrence_id='${original.occurrenceId}' AND ticket_id='${ticket.ticket}' AND occurred_at='${original.occurredAt}'::timestamptz AND decision='revocation_refused'`),'1');
      }else{assert.deepEqual(facts(ticket.ticket),[{occurrence_id:original.occurrenceId,event_type:'entry',occurred_at:original.occurredAt}]);assert.equal(quarantine(ticket.ticket),0);}
      const c=await context.newPage();await pair(c,token);await c.getByText(new RegExp(`Scan ${original.occurrenceId}.*Sync state: SYNCED`)).waitFor();
      await existing.evaluate(()=>window.dispatchEvent(new Event('online')));await existing.getByText(new RegExp(`Scan ${original.occurrenceId}.*Sync state: SYNCED`)).waitFor();
      check(`B5 ordinary expired-owner recovery after page close preserves ${kind}, exact reconciliation and cross-page notice`,true);
    }finally{await context.close();}
  }

}
