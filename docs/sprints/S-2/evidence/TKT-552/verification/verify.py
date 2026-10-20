import datetime, hashlib, json, os, pathlib, re, subprocess, sys, time
W=pathlib.Path('/home/mathieu/sources/ticketing_system.TKT-552-entry-copy')
P=pathlib.Path('/tmp/sdlc-S-2/TKT-552-primary-verification')
P.mkdir(exist_ok=True)
def now():return datetime.datetime.now(datetime.timezone.utc).isoformat()
def sha(data):return hashlib.sha256(data).hexdigest()
def git(*args):return subprocess.check_output(['git',*args],cwd=W,text=True).strip()
source=W/'web/scanner/src/scan-result.ts'
original=source.read_bytes()
head=git('rev-parse','HEAD')
assert head=='661bf5fb5cf58d1753643f9000e4475163a0e845' and not git('status','--porcelain')
assert not (W/'.gate.lock').exists()
records=[]
def run(label, args, expected=0, json_report=False):
    assert not (W/'.gate.lock').exists()
    report=P/(label+'.vitest.json')
    cmd=args+(['--reporter=verbose','--reporter=json','--outputFile.json='+str(report)] if json_report else [])
    start=now();tick=time.monotonic()
    out=P/(label+'.log')
    with out.open('w') as f:r=subprocess.run(cmd,cwd=W,env={**os.environ,'NO_COLOR':'1'},stdout=f,stderr=subprocess.STDOUT)
    rec={'label':label,'cwd':str(W),'command':cmd,'startedAt':start,'endedAt':now(),'seconds':time.monotonic()-tick,'exit':r.returncode,'head':head,'sourceSha256':sha(source.read_bytes()),'logSha256':sha(out.read_bytes())}
    if json_report:
        data=json.loads(report.read_text());failed=[{'file':t['name'],'title':a['fullName'],'messages':a['failureMessages']} for t in data['testResults'] for a in t['assertionResults'] if a['status']=='failed']
        rec.update(tests=data['numTotalTests'],passed=data['numPassedTests'],failed=data['numFailedTests'],failedAssertions=failed,reportSha256=sha(report.read_bytes()))
    records.append(rec);(P/'runs.json').write_text(json.dumps(records,indent=2)+'\n')
    print(json.dumps({k:rec[k] for k in ['label','exit','seconds','passed','failed'] if k in rec}),flush=True)
    assert r.returncode==expected,rec
    return rec
try:
    text=original.decode()
    exit_branch="      if (result.reason === 'exit_required') return refused('Rejected',\n        'Do not admit. Access records this pass as inside. Confirm that the holder has left, then record Exit before a new Entry scan.',\n        'Reason: exit_required.')\n"
    limit_branch="      if (result.reason === 'entry_limit_reached') return refused('Rejected', 'Do not admit. No entries remain on this pass.', 'Reason: entry_limit_reached.')\n"
    mutations=[('remove-exit-required-branch','exit_required',exit_branch,''),('remove-entry-limit-branch','entry_limit_reached',limit_branch,''),('remove-exit-required-reason','exit_required',",\n        'Reason: exit_required.'",''),('remove-entry-limit-reason','entry_limit_reached',", 'Reason: entry_limit_reached.'",'')]
    for label,reason,old,new in mutations:
        assert text.count(old)==1
        source.write_text(text.replace(old,new))
        (P/(label+'.patch')).write_text(subprocess.check_output(['git','diff','--','web/scanner/src/scan-result.ts'],cwd=W,text=True))
        try:
            rec=run(label,['pnpm','--filter','scanner','exec','vitest','run','src/scan-result.test.ts','src/App.test.tsx'],expected=1,json_report=True)
            assert rec['failed']==2 and len(rec['failedAssertions'])==2,rec
            assert all(reason in f['title'] and 'AssertionError' in '\n'.join(f['messages']) for f in rec['failedAssertions']),rec
            assert any('scan-result.test.ts' in f['file'] and ':44:' in '\n'.join(f['messages']) for f in rec['failedAssertions']),rec
            assert any('App.test.tsx' in f['file'] and ':130:' in '\n'.join(f['messages']) for f in rec['failedAssertions']),rec
        finally:
            source.write_bytes(original)
            restored={'observedAt':now(),'head':git('rev-parse','HEAD'),'sourceSha256':sha(source.read_bytes()),'clean':not git('status','--porcelain')}
            (P/(label+'-restored.json')).write_text(json.dumps(restored,indent=2)+'\n')
            assert restored['clean'] and restored['sourceSha256']==sha(original)
    for i in range(1,9):
        rec=run('restored-scanner-'+str(i),['pnpm','--filter','scanner','exec','vitest','run'],json_report=True)
        assert rec['failed']==0 and rec['passed']==rec['tests']
    run('scanner-lint',['pnpm','--filter','scanner','lint'])
    run('scanner-build',['pnpm','--filter','scanner','build'])
    assert not git('status','--porcelain')
    summary={'completedAt':now(),'head':head,'sourceSha256':sha(original),'mutantsKilled':4,'eachKilledBy':['exact presenter assertion','submitted App409 paragraph assertion'],'restoredRuns':8,'testCounts':[r['tests'] for r in records if r['label'].startswith('restored-scanner-')],'records':records}
    (P/'summary.json').write_text(json.dumps(summary,indent=2)+'\n')
    print(json.dumps({'complete':True,'mutantsKilled':4,'restoredRuns':8,'counts':summary['testCounts']}),flush=True)
finally:
    source.write_bytes(original)
