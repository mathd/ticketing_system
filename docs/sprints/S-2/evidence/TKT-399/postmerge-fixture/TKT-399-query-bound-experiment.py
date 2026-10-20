import subprocess,pathlib,json,datetime,hashlib
root=pathlib.Path('/home/mathieu/sources/ticketing_system.TKT-399-sync-fixture'); p=root/'web/scanner/src/App.exit-reconcile.test.tsx'; original=p.read_text();results=[]
injection="""  const queued = page.store().queued.bind(page.store())
  vi.spyOn(page.store(), 'queued').mockImplementationOnce(async () => {
    await new Promise(resolve => setTimeout(resolve, 1_200))
    return queued()
  })
"""
try:
 for name,bound in [('default-one-second',False),('fixture-two-second',True)]:
  source=original.replace('  render(<App />)',injection+'  render(<App />)',1)
  if bound:source=source.replace("{ name: 'Sync queued scans' }))", "{ name: 'Sync queued scans' }, { timeout: 2_000 }))",1)
  p.write_text(source)
  log=pathlib.Path('/tmp/sdlc-S-2/TKT-399-query-bound-'+name+'.log');start=datetime.datetime.now(datetime.timezone.utc).isoformat()
  with log.open('w') as f:r=subprocess.run(['pnpm','--filter','scanner','exec','vitest','run','src/App.exit-reconcile.test.tsx','-t','after transport'],cwd=root,stdout=f,stderr=subprocess.STDOUT)
  text=log.read_text();result={'case':name,'testSourceSha256':hashlib.sha256(source.encode()).hexdigest(),'delayMs':1200,'queryBoundMs':2000 if bound else 1000,'exit':r.returncode,'start':start,'end':datetime.datetime.now(datetime.timezone.utc).isoformat(),'log':str(log),'logSha256':hashlib.sha256(log.read_bytes()).hexdigest()};results.append(result);print(json.dumps(result),flush=True)
  assert (r.returncode==0) == bound
  if not bound:assert "Unable to find role=\"button\"" in text and 'App.exit-reconcile.test.tsx:50:' in text
finally:
 p.write_text(original)
 pathlib.Path('/tmp/sdlc-S-2/TKT-399-query-bound-experiment.json').write_text(json.dumps(results,indent=2)+'\n')
