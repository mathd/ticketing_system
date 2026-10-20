// Verifies the pinned inputs in inputs.json against the checkout.
//
// Usage: node lib/verify-inputs.mjs <repoRoot> <inputs.json>
// Prints one line per pin. Exit 1 when any pin is missing or differs.

import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const [root, manifestPath] = process.argv.slice(2);
const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
let mismatches = 0;
for (const pin of manifest.pinnedInputs) {
  let actual;
  try {
    actual = createHash('sha256').update(readFileSync(join(root, pin.path))).digest('hex');
  } catch (error) {
    actual = `missing:${error.code ?? 'error'}`;
  }
  const ok = actual === pin.sha256;
  if (!ok) mismatches += 1;
  console.log(`${ok ? 'OK' : 'MISMATCH'} ${actual} ${pin.path}`);
}
console.log(`pinned inputs: ${manifest.pinnedInputs.length}, mismatches: ${mismatches}`);
process.exitCode = mismatches === 0 ? 0 : 1;
