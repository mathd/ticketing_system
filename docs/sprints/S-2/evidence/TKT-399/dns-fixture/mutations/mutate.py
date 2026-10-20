#!/usr/bin/env python3
"""Run only after the parent commits and inspects the DNS fixture candidate."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import time

ROOT = Path('/home/mathieu/sources/ticketing_system.TKT-399-dns-fixture')
PRIMARY = Path('/home/mathieu/sources/ticketing_system')
OUT = Path('/tmp/sdlc-S-2/TKT-399-dns-implementation')
ENV = '/tmp/sdlc-S-2/dns-testdb-r3-20261020/environment.sh'
SCANNER = 'services/access/internal/testdb/scanner_smoke.go'
DNS = 'services/access/internal/testdb/dns_smoke.go'
TESTDB = './services/access/internal/testdb'
STORE = './services/access/internal/store'
SITES = ['device_lookup', 'policy_read', 'single_begin', 'pass_begin']
LOOKUP = 'TestRealConnectLookupDNSAtEverySite'
ATTEMPT = 'TestRealConnectAttemptDNSAtEverySite'
MIXED = 'TestRealMixedConnectLeavesUnderOneConnectError'
VARIANTS = ['mixed_supported', 'mixed_unsupported', 'mixed_unsupported_first']

def store_cases(name):
    return [f'{name}/{site}' for site in SITES]

def mixed_cases(variants):
    return [f'{MIXED}/{site}/{variant}' for site in SITES for variant in variants]

def spec(key, path, old, new, package, selector, cases, marker):
    return dict(id=key, path=path, old=old, new=new, package=package,
                selector=selector, expectedFailingTests=cases, assertion=marker)

mixed_selector = '^' + MIXED + '$'
all_dns_selector = '^(' + '|'.join([LOOKUP, ATTEMPT, MIXED]) + ')$'
all_dns_cases = store_cases(LOOKUP) + store_cases(ATTEMPT) + mixed_cases(VARIANTS)
mutants = [
 spec('01-lookup-default', SCANNER, 'return resolver.LookupHost(ctx, invalid)',
      'return net.DefaultResolver.LookupHost(ctx, invalid)', STORE,
      '^'+LOOKUP+'$', store_cases(LOOKUP), 'no-owned-wire-questions'),
 spec('02-attempt-default', SCANNER, 'faultDialer := &net.Dialer{Resolver: resolver}',
      'faultDialer := &net.Dialer{}', STORE, '^'+ATTEMPT+'$',
      store_cases(ATTEMPT), 'no-owned-wire-questions'),
 spec('03-mixed-default', SCANNER, 'cc.LookupFunc = resolver.LookupHost',
      'cc.LookupFunc = net.DefaultResolver.LookupHost', STORE, mixed_selector,
      mixed_cases(VARIANTS), 'no-owned-wire-questions'),
 spec('04-wrong-request-name', SCANNER,
      'refused, blocked, invalid := f.refusedAddr, f.blocked, f.invalid',
      'refused, blocked, invalid := f.refusedAddr, f.blocked, f.invalid\n\tinvalid = "different-" + invalid',
      STORE, all_dns_selector, all_dns_cases, 'unexpected DNS question'),
 spec('05-relative-request-name', SCANNER,
      'refused, blocked, invalid := f.refusedAddr, f.blocked, f.invalid',
      'refused, blocked, invalid := f.refusedAddr, f.blocked, f.invalid\n\tinvalid = strings.TrimSuffix(invalid, ".")',
      STORE, all_dns_selector, all_dns_cases, 'DNS properties:'),
 spec('06-servfail', DNS, 'RCode: dnsmessage.RCodeNameError',
      'RCode: dnsmessage.RCodeServerFailure', TESTDB, '^TestDNSProducerRealResolver$',
      ['TestDNSProducerRealResolver/LookupHost', 'TestDNSProducerRealResolver/DialContext'],
      'DNS properties:'),
 spec('07-remove-unsupported-hop', SCANNER,
      'cc.Host, cc.Port, cc.Fallbacks = hops[0].Host, hops[0].Port, hops[1:]',
      'if fault != FaultMixedSupported { hops = []*pgconn.FallbackConfig{recoveryHop, lookupHop} }\n\t\tcc.Host, cc.Port, cc.Fallbacks = hops[0].Host, hops[0].Port, hops[1:]',
      STORE, mixed_selector+'/^('+ '|'.join(SITES) +')$/^(mixed_unsupported|mixed_unsupported_first)$',
      mixed_cases(VARIANTS[1:]), 'mixed socket leaf missing:'),
 spec('08-remove-recovery-hop', SCANNER,
      'cc.Host, cc.Port, cc.Fallbacks = hops[0].Host, hops[0].Port, hops[1:]',
      'kept := hops[:0]\n\t\tfor _, hop := range hops { if hop != recoveryHop { kept = append(kept, hop) } }\n\t\thops = kept\n\t\tcc.Host, cc.Port, cc.Fallbacks = hops[0].Host, hops[0].Port, hops[1:]',
      STORE, mixed_selector, mixed_cases(VARIANTS), 'mixed 57P03 leaf missing:'),
 spec('09-same-socket-order', SCANNER,
      'hops = []*pgconn.FallbackConfig{otherHop, lookupHop, recoveryHop}',
      'hops = []*pgconn.FallbackConfig{recoveryHop, lookupHop, otherHop}',
      STORE, mixed_selector+'/^('+ '|'.join(SITES) +')$/^mixed_unsupported_first$',
      mixed_cases(['mixed_unsupported_first']), 'mixed 57P03/socket order:'),
 spec('10-omit-active-close', DNS,
      'for _, conn := range sockets {\n\t\t\t_ = conn.Close()\n\t\t}',
      'for _, conn := range sockets {\n\t\t\t_ = conn\n\t\t}',
      TESTDB, '^TestDNSProducerClosePartialFrame$', ['TestDNSProducerClosePartialFrame'],
      'Close did not terminate the partial-frame handler within 1s'),
]

CANDIDATE_FILES = [SCANNER, DNS, 'services/access/internal/testdb/dns_smoke_test.go',
                   'services/access/internal/store/scanner_availability_smoke_test.go', 'services/access/go.mod']

def digest(data):
    return hashlib.sha256(data).hexdigest()

def git(*args):
    return subprocess.check_output(['git', *args], cwd=ROOT, text=True).strip()

def no_gate():
    for root in (ROOT, PRIMARY):
        if os.path.lexists(root / '.gate.lock'):
            raise RuntimeError(f'refuse active gate lock: {root / ".gate.lock"}')

def clean():
    no_gate()
    if git('status', '--porcelain', '--untracked-files=no'):
        raise RuntimeError('refuse dirty tracked candidate; parent must inspect and commit first')
    if git('branch', '--show-current') != 'TKT-399-dns-fixture':
        raise RuntimeError('wrong candidate branch')
    if git('rev-parse', 'HEAD') == '2c8b35bbad7f24a863e7ef7ef8b9c89613931174':
        raise RuntimeError('candidate is not committed')
    for path in CANDIDATE_FILES:
        git('ls-files', '--error-unmatch', path)


def run(case, count, directory, label):
    no_gate()
    args = ['go', 'test', '-json', '-tags', 'smoke', f'-count={count}', '-timeout=3m', case['package'], '-run', case['selector']]
    command = f'source {shlex.quote(ENV)}\n' + shlex.join(args)
    started = datetime.datetime.now(datetime.timezone.utc).isoformat()
    clock = time.monotonic()
    log_path = directory / (label + '.jsonl')
    # Never print or persist the shell environment or the DSNs.
    with log_path.open('wb') as log:
        proc = subprocess.Popen(['bash', '-c', command], cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        interrupted = None
        try:
            code = proc.wait(timeout=240)
        except BaseException as error:
            interrupted = error
            os.killpg(proc.pid, signal.SIGTERM)
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
            code = proc.returncode
    text = log_path.read_text()
    record = dict(command=command, cwd=str(ROOT), start=started,
                  end=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  seconds=time.monotonic()-clock, exit=code, log=str(log_path), sha256=digest(log_path.read_bytes()))
    (directory/(label+'.json')).write_text(json.dumps(record, indent=2)+'\n')
    if interrupted is not None:
        raise interrupted
    outputs, actions = {}, {}
    for line in text.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        name = event.get('Test', '')
        outputs[name] = outputs.get(name, '') + event.get('Output', '')
        actions.setdefault(name, []).append(event.get('Action'))
    if any('skip' in events for events in actions.values()):
        raise RuntimeError(f'{label}: a skipped test is not evidence')
    if count == 1:
        if code == 0:
            raise RuntimeError(f'{label}: surviving mutant')
        for name in case['expectedFailingTests']:
            if 'fail' not in actions.get(name, []) or case['assertion'] not in outputs.get(name, ''):
                raise RuntimeError(f'{label}: {name} did not fail at its owning assertion {case["assertion"]!r}')
    else:
        if code != 0:
            raise RuntimeError(f'{label}: restored candidate failed; stop and diagnose')
        for name in case['expectedFailingTests']:
            if actions.get(name, []).count('pass') != 8:
                raise RuntimeError(f'{label}: {name} did not pass exactly eight restored runs')
    print(json.dumps(record), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--list', action='store_true', help='Print mutation selectors without editing or testing')
    parser.add_argument('--run', action='store_true', help='Parent only, after candidate commit')
    opts = parser.parse_args()
    if opts.list:
        print(json.dumps(mutants, indent=2))
        return
    if not opts.run:
        parser.error('use --list, or --run after parent inspection and commit')
    clean()
    pinned = json.loads((OUT/'candidate-code-sha256.json').read_text())
    for path in CANDIDATE_FILES:
        if digest((ROOT/path).read_bytes()) != pinned[path]:
            raise RuntimeError(f'candidate bytes changed: {path}; update the driver after inspecting the new fixture')
    head = git('rev-parse', 'HEAD')
    directory = OUT / ('mutations-' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ'))
    directory.mkdir()
    originals = {path:(ROOT/path).read_bytes() for path in CANDIDATE_FILES}
    for path, data in originals.items():
        backup = directory/'originals'/path
        backup.parent.mkdir(parents=True, exist_ok=True)
        backup.write_bytes(data)
    (directory/'candidate.json').write_text(json.dumps(dict(head=head, tree=git('rev-parse','HEAD^{tree}'), sha256=pinned),indent=2)+'\n')
    for case in mutants:
        clean()
        if git('rev-parse','HEAD') != head:
            raise RuntimeError('HEAD changed during mutation run')
        path = ROOT/case['path']
        original = originals[case['path']]
        old, new = case['old'].encode(), case['new'].encode()
        if original.count(old) != 1:
            raise RuntimeError(f'{case["id"]}: mutation target is not unique')
        try:
            no_gate()
            path.write_bytes(original.replace(old,new))
            run(case,1,directory,case['id']+'-red')
        finally:
            path.write_bytes(original)
            if path.read_bytes() != original:
                raise RuntimeError('byte restoration failed')
        clean()
        run(case,8,directory,case['id']+'-restored-8')
    (directory/'complete.json').write_text(json.dumps(dict(head=head,mutants=10,restoredRunsPerMutant=8),indent=2)+'\n')

def interrupted_signal(signum, frame):
    raise KeyboardInterrupt(f'interrupted by signal {signum}')

if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupted_signal)
    try:
        main()
    except Exception as error:
        print(f'STOP: {error}', file=sys.stderr)
        sys.exit(1)
