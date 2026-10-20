import pathlib, subprocess, json, datetime, hashlib, time, sys
sys.path.insert(0, '/tmp/sdlc-S-2')
import board
w = pathlib.Path('/home/mathieu/sources/ticketing_system.TKT-551-sync-report')
p = pathlib.Path('/tmp/sdlc-S-2/TKT-551-review-fix-verification')
p.mkdir(exist_ok=True)
app = w / 'web/scanner/src/App.tsx'
sync = w / 'web/scanner/src/App.sync.test.tsx'
legacy = w / 'web/scanner/src/App.legacy.test.tsx'
original = {f: f.read_bytes() for f in [app, sync, legacy]}
head = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=w, text=True).strip()
assert not subprocess.check_output(['git', 'status', '--porcelain'], cwd=w, text=True)
assert not (w / '.gate.lock').exists()

def stamp(): return datetime.datetime.now(datetime.timezone.utc).isoformat()

def run(name, args, expected=0, anchor=None):
    start = stamp(); clock = time.monotonic(); log = p / (name + '.log')
    with log.open('w') as out:
        r = subprocess.run(args, cwd=w, stdout=out, stderr=subprocess.STDOUT, timeout=180)
    body = log.read_text()
    row = {'operation': name, 'ticket': 'TKT-551', 'start': start, 'end': stamp(),
           'seconds': time.monotonic() - clock, 'exitCode': r.returncode,
           'expectedExit': expected, 'head': head, 'command': args,
           'receipt': str(log), 'logSha256': hashlib.sha256(log.read_bytes()).hexdigest()}
    with (p / 'runs.jsonl').open('a') as out: out.write(json.dumps(row) + '\n')
    board.observe(name, 'TKT-551', start, row, str(log))
    print(json.dumps({'name': name, 'exit': r.returncode, 'seconds': row['seconds']}), flush=True)
    assert r.returncode == expected, (name, r.returncode)
    if expected:
        assert 'Failed Tests' in body and 'Unhandled Errors' not in body, name
        assert anchor and anchor in body, (name, anchor)
    return row

def change(file, old, new):
    source = original[file].decode()
    assert source.count(old) == 1, (old, source.count(old))
    file.write_text(source.replace(old, new))

def restore():
    for f, data in original.items(): f.write_bytes(data)
    assert not subprocess.check_output(['git', 'diff', '--name-only'], cwd=w, text=True)

base = ['pnpm', '--filter', 'scanner', 'test']
try:
    run('focused-green', base + ['src/App.sync.test.tsx', 'src/App.legacy.test.tsx', 'src/App.reconcile.test.tsx'])
    run('build', ['pnpm', '--filter', 'scanner', 'build'])
    run('lint', ['pnpm', '--filter', 'scanner', 'lint'])
    empty = ['src/App.sync.test.tsx', '-t', 'keeps the final report and rejection warning']
    mutations = [
        ('M16-empty-read-warning-retention', app, '    if (!queue.length) {',
         '    if (!queue.length) {\n      setReconcileWarnings([])', empty,
         'expect(screen.getByText(`Reconciliation rejected'),
        ('M17-empty-read-final-note-retention', app, '    if (!queue.length) {',
         '    if (!queue.length) {\n      setSyncNote(null)', empty,
         'expect(screen.getByText(note))'),
        ('M18-exit-fixture-observation', sync, "index % 3 === 1 ? 'entry' : 'exit'",
         "index % 3 === 1 ? 'entry' : 'entry'",
         ['src/App.sync.test.tsx', '-t', 'counts every result.*false'],
         'expect(before[98]).toMatchObject'),
        ('M19-refusal-fixture-observation', sync, "index % 3 === 0 ? 'refusal'",
         "index % 3 === 0 ? 'entry'",
         ['src/App.sync.test.tsx', '-t', 'counts every result.*false'],
         'expect(before[99]).toMatchObject'),
    ]
    for name, file, old, new, args, anchor in mutations:
        change(file, old, new)
        try: run(name, base + args, 1, anchor)
        finally: restore()

    old = "        if (!response.ok) { void reportSyncFailure('unreadable'); return }"
    new = "        if (!response.ok) { warnings.push('Reconciliation rejected for c-isolated. This result is not recorded.'); publishWarnings(); void reportSyncFailure('unreadable'); return }"
    args = base + ['src/App.legacy.test.tsx', '-t', 'keeps completed-batch counts separate']
    change(app, old, new)
    change(legacy, "screen.getAllByRole('alert').map(node => node.textContent)",
           "screen.getAllByRole('alert').slice(0, 3).map(node => node.textContent)")
    try:
        run('M20-old-slice-misses-extra-warning', args)
        legacy.write_bytes(original[legacy])
        run('M20-exact-alert-list-catches-extra-warning', args, 1, "getAllByRole('alert').map")
    finally: restore()

    for index in range(1, 9):
        args = base[:]
        if index == 1:
            args += ['--reporter=default', '--reporter=json', '--outputFile=' + str(p / 'restored-full-01.json')]
        run(f'restored-full-{index:02d}', args)
    report = json.loads((p / 'restored-full-01.json').read_text())
    timings = []
    for suite in report['testResults']:
        for test in suite.get('assertionResults', []):
            if 'aggregate reconciliation reporting' in test.get('fullName', ''):
                timings.append({k: test.get(k) for k in ['fullName', 'status', 'duration']})
    assert timings and all(t['status'] == 'passed' and isinstance(t['duration'], (int, float)) for t in timings)
    result = {'head': head, 'vitest': '4.1.10', 'testBudgetMs': 15000,
              'perWaitBoundMs': 2000, 'source': 'restored-full-01.json',
              'scope': 'aggregate cases under full 43-file scanner-suite load',
              'timings': timings, 'maximumCaseMs': max(t['duration'] for t in timings),
              'note': 'Per-test durations, not individual wait timings. No test bound or production deadline changed.'}
    (p / 'aggregate-test-timings.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result), flush=True)
finally:
    restore()
assert not subprocess.check_output(['git', 'status', '--porcelain'], cwd=w, text=True)
print('PASS: five targeted mutations killed at their intended assertions; old legacy slice demonstrated blind; eight restored full scanner suites; exact source restore.', flush=True)
