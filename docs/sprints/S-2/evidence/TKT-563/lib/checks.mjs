// Named check recorder for the TKT-563 probe.
//
// A check records a result and never throws. A journey keeps reading the
// database after a wire mismatch, so the readback exists before any verdict.
// Every detail string is scrubbed before it is kept, printed or written.

// Shape of a signed ticket credential: three base64url segments.
const CREDENTIAL_SHAPE = /[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}/g;

export function createScrubber() {
  const values = new Set();
  return {
    add(value) {
      if (typeof value === 'string' && value.length >= 8) values.add(value);
    },
    scrub(text) {
      let out = typeof text === 'string' ? text : JSON.stringify(text);
      for (const value of values) out = out.split(value).join('[REDACTED]');
      return out.replace(CREDENTIAL_SHAPE, '[REDACTED]');
    },
  };
}

export function createChecks(scrubber) {
  const list = [];
  return {
    check(id, ok, detail) {
      const entry = { id, ok: ok === true };
      if (detail !== undefined) entry.detail = scrubber.scrub(detail).slice(0, 2000);
      list.push(entry);
      return entry.ok;
    },
    list,
    failedIds() {
      return [...new Set(list.filter((entry) => !entry.ok).map((entry) => entry.id))];
    },
  };
}
