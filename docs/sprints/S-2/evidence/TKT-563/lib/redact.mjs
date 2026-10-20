// Stdin-to-stdout redaction for every log the runner keeps.
//
// It replaces the value of each secret-named environment variable, and with
// --generic it also replaces long hex runs and base64url credential triples.
// Commit hashes of 40 hex digits stay readable.

const NAME = /(TOKEN|KEY|SEED|PASSWORD|SECRET|PRIVATE)/;
const generic = process.argv.includes('--generic');

const values = Object.entries(process.env)
  .filter(([name, value]) => NAME.test(name) && typeof value === 'string' && value.length >= 8)
  .map(([, value]) => value);

const HEX_RUN = /\b[0-9a-fA-F]{48,}\b/g;
const CREDENTIAL_TRIPLE = /[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/g;

let input = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', (chunk) => {
  input += chunk;
});
process.stdin.on('end', () => {
  let out = input;
  for (const value of values) out = out.split(value).join('[REDACTED]');
  if (generic) out = out.replace(HEX_RUN, '[REDACTED]').replace(CREDENTIAL_TRIPLE, '[REDACTED]');
  process.stdout.write(out);
});
