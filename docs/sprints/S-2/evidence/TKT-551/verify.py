import pathlib,subprocess,json,datetime,hashlib,time,sys
w=pathlib.Path('/home/mathieu/sources/ticketing_system.TKT-551-sync-report')
p=pathlib.Path('/tmp/sdlc-S-2/TKT-551-verification')
app=w/'web/scanner/src/App.tsx';store=w/'web/scanner/src/occurrence-store.ts'
original={f:f.read_bytes() for f in [app,store]}
head=subprocess.check_output(['git','rev-parse','HEAD'],cwd=w,text=True).strip()
assert not subprocess.check_output(['git','status','--porcelain'],cwd=w,text=True)
def stamp():return datetime.datetime.now(datetime.timezone.utc).isoformat()
def run(name,args,expected):
    start=stamp();clock=time.monotonic();log=p/(name+'.log')
    with log.open('w') as out:r=subprocess.run(args,cwd=w,stdout=out,stderr=subprocess.STDOUT,timeout=180)
    body=log.read_text();row={'operation':name,'ticket':'TKT-551','start':start,'end':stamp(),'seconds':time.monotonic()-clock,'result':r.returncode,'expectedExit':expected,'head':head,'receipt':str(log),'logSha256':hashlib.sha256(log.read_bytes()).hexdigest(),'command':args}
    for target in [p/'runs.jsonl',pathlib.Path('/tmp/sdlc-S-2-operations.jsonl')]:
        with target.open('a') as out:out.write(json.dumps(row)+'\n')
    print(json.dumps({k:row[k] for k in ['operation','result','seconds']}),flush=True)
    assert r.returncode==expected,(name,r.returncode)
    if expected:
        assert 'Failed Tests' in body and ' FAIL ' in body and 'Unhandled Errors' not in body,name+' did not fail as an assertion'
    else:assert 'Test Files' in body and 'Tests ' in body,name+' missing test receipt'

def one(text,old,new):
    assert text.count(old)==1,(old,text.count(old))
    return text.replace(old,new)

text=original[app].decode();storetext=original[store].decode()
full=['src/App.sync.test.tsx','-t','counts every result.*false']
partial=['src/App.sync.test.tsx','-t','retains applied-batch totals.*transport']
single=['src/App.sync.test.tsx','-t','reports a single rejected result']
legacy=['src/App.legacy.test.tsx','-t','keeps completed-batch counts separate']
mutations=[]
def mutation(name,old,new,args=full,file=app):
    mutations.append((name,file,one(original[file].decode(),old,new),args))
mutation('M01-recorded-counter','recorded += 1','recorded += 2')
mutation('M02-synced-alternative',"result.result === 'recorded' || result.result === 'synced'","result.result === 'recorded'")
mutation('M03-conflict-counter','conflicts += 1','conflicts += 2')
mutation('M04-rejected-counter','rejected += 1','rejected += 2')
mutation('M05-batch-accumulation','    for (const batch of plan.batches) {','    for (const batch of plan.batches) {\n      recorded = 0; conflicts = 0; rejected = 0')
mutation('M06-rejection-warning','          warnings.push(`Reconciliation rejected for ${result.occurrence_id}. This result is not recorded.`)','          void result.occurrence_id')
mutation('M07-rejected-retirement',"const retired = result === 'recorded' || result === 'synced' ? 'SYNCED' : 'RESOLVED'", "const retired = result === 'recorded' || result === 'synced' || result === 'rejected' ? 'SYNCED' : 'RESOLVED'",single,store)
mutation('M08-partial-publication','recorded + conflicts + rejected > 0','false',partial)
mutation('M09-partial-removal','      setReconcileWarnings(warnings)','      publishWarnings()')
start=text.index('      try {\n        const applied = await getStore().applyReconcileResults')
end=text.index('\n    }\n    if (mounted.current && isPairing(captured))',start)
block=text[start:end];split=block.index('      for (const result of data.results) {')
mutations.append(('M10-publish-before-apply',app,one(text,block,block[split:]+'\n'+block[:split].rstrip()),['src/App.sync.test.tsx','-t','retains applied-batch totals.*write']))
formatter=next(line for line in text.splitlines() if 'const aggregateText = ' in line)
mutation('M11-warning-count-source',formatter,formatter.replace('${rejected','${warnings.length'),['src/App.legacy.test.tsx','-t','sends literal legacy direction classes'])
mutation('M12-partial-after-isolated-422',"          publishWarnings()\n          continue", "          if (mounted.current && isPairing(captured)) setReconcileWarnings([...warnings])\n          continue",legacy)
mutation('M13-next-pass-reset','    publishWarnings()\n    for (const batch','    for (const batch',['src/App.sync.test.tsx','-t','replaces the partial report at automatic retry start'])
old='        ...(recorded + conflicts + rejected > 0 ? [`Completed batches: ${aggregateText()}`] : []), ...warnings,'
new='        ...warnings, ...(recorded + conflicts + rejected > 0 ? [`Completed batches: ${aggregateText()}`] : []),'
mutation('M14-partial-alert-order',old,new,partial)
mutation('M15-singular-rejection-copy',"rejected === 1 ? ' was' : 's were'","rejected === 1 ? ' were' : 's were'",single)
try:
    for name,file,mutant,args in mutations:
        file.write_text(mutant)
        try:run(name,['pnpm','--filter','scanner','test',*args],1)
        finally:
            for f,body in original.items():f.write_bytes(body)
        assert not subprocess.check_output(['git','diff','--name-only'],cwd=w,text=True)
    for index in range(1,9):run(f'restored-full-{index:02d}',['pnpm','--filter','scanner','test'],0)
finally:
    for f,body in original.items():f.write_bytes(body)
assert not subprocess.check_output(['git','status','--porcelain'],cwd=w,text=True)
print('PASS: 15 assertion-killed mutations and eight restored full scanner suites; source restored exactly.',flush=True)
