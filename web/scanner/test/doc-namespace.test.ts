import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'
const root = execFileSync('git', ['rev-parse', '--show-toplevel'], { encoding: 'utf8' }).trim()
const decision = /\bD[1-9][0-9]*(?:-[0-9]+)?[a-z]?\b/g
const review = /\bP[RA][0-9]+(?:-[0-9]+)?\b/g
function scannerOperatorGuide(): string {
  const guide = readFileSync(`${root}/docs/development.md`, 'utf8')
  const start = guide.indexOf('\n### Scanner Entry outcomes and local records\n')
  expect(start).toBeGreaterThan(-1)
  const end = guide.indexOf('\n## ', start + 1)
  expect(end).toBeGreaterThan(start)
  return guide.slice(start, end)
}
it('DOC-01 keeps decision and review identifiers out of the operator guide', () => {
  const guide = scannerOperatorGuide()
  expect(guide.match(decision)).toBeNull()
  expect(guide.match(review)).toBeNull()
})
it('DOC-01 qualifies every decision identifier added to an ADR', () => {
  const diff = execFileSync('git', ['diff', 'ec336db814d719130cdefd263ca84be73b9f06e2', '--', 'docs/adr/'], { cwd: root, encoding: 'utf8' })
  const added = diff.split('\n').filter(line => line.startsWith('+') && !line.startsWith('+++'))
  const unqualified: string[] = []
  for (const line of added) {
    for (const match of line.matchAll(decision)) {
      const prefix = line.slice(0, match.index)
      if (!prefix.endsWith('TKT-558 ') && !prefix.endsWith('ADR-085 ')) unqualified.push(line)
    }
  }
  expect(unqualified).toEqual([])
})

it('DOC-01 retains the interim no-clock and slow-refusal warnings', () => {
  const guide = scannerOperatorGuide()
  expect(guide).toContain('There is no scan clock or stalled-body deadline until TKT-559.')
  expect(guide).toContain('timeout can still select the network fallback until the deadline work ships.')
  expect(guide).toContain('A failed terminal write followed by actual page loss can still recover as a false admission.')
})
