import { describe, expect, it } from 'vitest';

import { dateTimeField, uuidField } from '../src/lib/wire-primitives';

describe('wire identities', () => {
  it('rejects the nil UUID even though its spelling is valid', () => {
    expect(() => uuidField('00000000-0000-0000-0000-000000000000', 'customer_id')).toThrow(
      'customer_id must be a non-nil UUID',
    );
  });
});

describe('RFC 3339 wire dates', () => {
  it.each([
    '2024-02-29T23:59:59Z',
    '2024-02-29t23:59:59z',
    '2026-09-04T12:30:45.123456+05:30',
    '2000-02-29T00:00:00-00:45',
  ])('accepts %s', (value) => {
    expect(dateTimeField(value, 'timestamp')).toBe(value);
  });

  it.each([
    '2025-02-29T00:00:00Z',
    '1900-02-29T00:00:00Z',
    '2026-04-31T00:00:00Z',
    '2026-00-01T00:00:00Z',
    '2026-01-01T24:00:00Z',
    '2026-01-01T00:60:00Z',
    '2026-01-01T00:00:60Z',
    '2016-12-31T23:59:60Z',
    '2026-01-01T00:00:00+24:00',
  ])('rejects %s', (value) => {
    expect(() => dateTimeField(value, 'timestamp')).toThrow('timestamp must be an RFC 3339 date-time');
  });
});
