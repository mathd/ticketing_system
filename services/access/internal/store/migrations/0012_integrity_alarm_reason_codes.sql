-- +goose Up
ALTER TABLE lifecycle_integrity_quarantine
  ADD COLUMN reason_code text
  CHECK (reason_code IS NULL OR reason_code IN (
    'missing_integrity_row',
    'unsupported_canonical_version',
    'sequence_gap',
    'broken_chain_link',
    'entry_hash_mismatch',
    'orphan_integrity_row',
    'missing_head',
    'head_without_events',
    'head_mismatch',
    'missing_keyring',
    'unknown_key',
    'invalid_head_signature',
    'verification_unavailable',
    'verification_unclassified',
    'legacy_quarantine'
  ));

-- +goose Down
ALTER TABLE lifecycle_integrity_quarantine DROP COLUMN reason_code;
