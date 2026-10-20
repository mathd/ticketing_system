// Applies one presenter or counter mutation to a disposable copy of web/scanner.
// It runs only on the copy under /tmp, never on the worktree.
//
// Usage: node lib/mutate-source.mjs <scannerCopyDir> <variant> <receiptFile>
// Exit 1 when the anchor does not match exactly once, so no mutant can be a no-op.

import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { createHash } from 'node:crypto';

const MUTATIONS = {
  // Exit denial: the exit_required branch returns the generic denial copy.
  'COPY-EXIT': {
    file: 'src/scan-result.ts',
    pattern: /if \(result\.reason === 'exit_required'\) return refused\('Rejected',[\s\S]*?'Reason: exit_required\.'\)/g,
    replacement: "if (result.reason === 'exit_required') return refused('Rejected', 'Credential is invalid or cannot be redeemed.')",
  },
  // Limit denial: the entry_limit_reached branch returns the generic denial copy.
  'COPY-LIMIT': {
    file: 'src/scan-result.ts',
    pattern: /if \(result\.reason === 'entry_limit_reached'\) return refused\('Rejected', 'Do not admit\. No entries remain on this pass\.', 'Reason: entry_limit_reached\.'\)/g,
    replacement: "if (result.reason === 'entry_limit_reached') return refused('Rejected', 'Credential is invalid or cannot be redeemed.')",
  },
  // Counter: the rejected aggregate never increases, so the summary omits it.
  'COUNT-REJECTED': {
    file: 'src/App.tsx',
    pattern: /rejected \+= 1/g,
    replacement: 'rejected += 0',
  },
};

const [scannerDir, variant, receiptPath] = process.argv.slice(2);
const spec = MUTATIONS[variant];
if (!spec || !scannerDir || !receiptPath) {
  console.error('usage: node lib/mutate-source.mjs <scannerCopyDir> <variant> <receiptFile>');
  process.exit(2);
}

const target = join(scannerDir, spec.file);
const before = readFileSync(target, 'utf8');
const matches = before.match(spec.pattern) ?? [];
if (matches.length !== 1) {
  console.error(`mutation ${variant}: anchor matched ${matches.length} times in ${spec.file}; expected exactly 1`);
  process.exit(1);
}

// Keep the original next to the copy so the caller can write an exact diff.
const originals = join(dirname(scannerDir), 'originals');
mkdirSync(originals, { recursive: true });
copyFileSync(target, join(originals, basename(spec.file)));

const after = before.replace(spec.pattern, spec.replacement);
writeFileSync(target, after);

const sha = (text) => createHash('sha256').update(text).digest('hex');
writeFileSync(
  receiptPath,
  `${JSON.stringify(
    {
      variant,
      file: spec.file,
      anchorMatches: 1,
      beforeSha256: sha(before),
      afterSha256: sha(after),
      replacement: spec.replacement,
    },
    null,
    2,
  )}\n`,
);
