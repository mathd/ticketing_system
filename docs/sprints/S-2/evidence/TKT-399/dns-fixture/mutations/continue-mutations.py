#!/usr/bin/env python3
"""Continue the retained run after the relative-name assertion was shadowed."""
import importlib.util
import json
import signal
import sys
from pathlib import Path

OUT = Path('/tmp/sdlc-S-2/TKT-399-dns-implementation')
spec = importlib.util.spec_from_file_location('dns_mutations', OUT / 'mutate.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
directory = OUT / 'mutations-20261020T040840.441376Z'
m.clean()
candidate = json.loads((directory / 'candidate.json').read_text())
assert m.git('rev-parse', 'HEAD') == candidate['head']
originals = {path: (m.ROOT / path).read_bytes() for path in m.CANDIDATE_FILES}
for path, data in originals.items():
    assert m.digest(data) == candidate['sha256'][path], path
for case in m.mutants[:4]:
    for suffix in ['-red', '-restored-8']:
        record = json.loads((directory / (case['id'] + suffix + '.json')).read_text())
        assert m.digest(Path(record['log']).read_bytes()) == record['sha256']
        assert record['exit'] == (1 if suffix == '-red' else 0)

# The original fifth mutant reached a stronger, earlier exact-name assertion.
case = m.mutants[4]
record = json.loads((directory / (case['id'] + '-red.json')).read_text())
assert record['exit'] == 1
assert m.digest(Path(record['log']).read_bytes()) == record['sha256']
outputs, failed = {}, set()
for line in Path(record['log']).read_text().splitlines():
    try:
        event = json.loads(line)
    except ValueError:
        continue
    name = event.get('Test', '')
    outputs[name] = outputs.get(name, '') + event.get('Output', '')
    if event.get('Action') == 'fail':
        failed.add(name)
for name in case['expectedFailingTests']:
    assert name in failed and 'unexpected DNS question' in outputs[name], name
(directory / '05-observed-assertion.json').write_text(json.dumps({
    'originalDriverStopped': True,
    'expectedMarker': case['assertion'],
    'observedMarker': 'unexpected DNS question',
    'cause': 'Host search suffix was added to the relative name; wire assertion ran before DNSError properties.',
    'failingCases': len(case['expectedFailingTests']),
    'sourceChanged': False,
    'extraProof': '05b changes only the direct helper LookupHost request input, leaving its absolute expected name unchanged. DNSError properties are asserted before wire checks there.'
}, indent=2) + '\n')
m.run(case, 8, directory, case['id'] + '-restored-8')

extra = m.spec('05b-relative-helper-input',
    'services/access/internal/testdb/dns_smoke_test.go',
    '_, err = p.resolver.LookupHost(ctx, p.name)',
    '_, err = p.resolver.LookupHost(ctx, strings.TrimSuffix(p.name, "."))',
    m.TESTDB, '^TestDNSProducerRealResolver$/^LookupHost$',
    ['TestDNSProducerRealResolver/LookupHost'], 'DNS properties:')

for case in [extra, *m.mutants[5:]]:
    m.clean()
    assert m.git('rev-parse', 'HEAD') == candidate['head']
    path = m.ROOT / case['path']
    original = originals[case['path']]
    old, new = case['old'].encode(), case['new'].encode()
    assert original.count(old) == 1, case['id']
    try:
        m.no_gate()
        path.write_bytes(original.replace(old, new))
        m.run(case, 1, directory, case['id'] + '-red')
    finally:
        path.write_bytes(original)
        assert path.read_bytes() == original
    m.clean()
    m.run(case, 8, directory, case['id'] + '-restored-8')
(directory / 'complete.json').write_text(json.dumps({
    'head': candidate['head'], 'mutantFamilies': 10, 'additionalDirectNameMutant': 1,
    'restoredRunsPerMutant': 8, 'originalDriverStopRetained': True,
    'explanation': '05 main-store mutation failed earlier at exact wire-name assertion. 05b separately killed the absolute DNSError.Name input mutation.'
}, indent=2) + '\n')

