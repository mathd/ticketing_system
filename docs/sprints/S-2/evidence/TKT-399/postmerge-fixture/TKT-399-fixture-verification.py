import subprocess,pathlib,json,datetime,hashlib
root=pathlib.Path('/home/mathieu/sources/ticketing_system.TKT-399-sync-fixture');out=pathlib.Path('/tmp/sdlc-S-2');receipts=[]
def run(name,args):
 log=out/('TKT-399-fixture-'+name+'.log');start=datetime.datetime.now(datetime.timezone.utc).isoformat()
 with log.open('w') as f:r=subprocess.run(args,cwd=root,stdout=f,stderr=subprocess.STDOUT)
 receipt={'case':name,'command':args,'exit':r.returncode,'start':start,'end':datetime.datetime.now(datetime.timezone.utc).isoformat(),'log':str(log),'logSha256':hashlib.sha256(log.read_bytes()).hexdigest()};receipts.append(receipt);(out/'TKT-399-fixture-verification.json').write_text(json.dumps(receipts,indent=2)+'\n');print(name,r.returncode,flush=True);return r.returncode,log.read_text()
args=['pnpm','--filter','scanner','exec','vitest','run','src/App.exit-reconcile.test.tsx']
p=root/'web/scanner/src/reconcile-batch.ts';original=p.read_text();assert original.count('else row.event_type = record.direction')==1
try:
 p.write_text(original.replace('else row.event_type = record.direction',"else row.event_type = 'entry'"))
 code,text=run('mutation-exit-as-entry',args+['-t','after transport']);assert code!=0 and 'toStrictEqual' in text and 'event_type' in text and 'exit' in text
finally:p.write_text(original)
for n in range(1,9):
 code,_=run('restored-'+str(n),args);assert code==0
code,_=run('full-scanner',['pnpm','--filter','scanner','test']);assert code==0
code,_=run('typescript',['pnpm','--filter','scanner','exec','tsc','--noEmit']);assert code==0
