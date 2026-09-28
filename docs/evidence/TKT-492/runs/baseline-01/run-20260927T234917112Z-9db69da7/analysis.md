# TKT-492 analysis

Profile: `tkt492-local-scanner-v1`

## Attempt 01

Result: **profile_meets_target**
Generator: valid. Correctness: pass. Latency: pass.

Measured samples: 2000. Maximum: 74.29999995231628 ms. Samples at or above 500 ms: 0.
Latency p50/p95/p99: 40/53.199999928474426/59.59999990463257 ms.
Scheduler lag p99/max: 0.3998534679412842/6.100000023841858 ms.
Validity: missed=0, overlap=0, unknown transport=0.

Counts:

```json
{
  "single_accept": 400,
  "single_duplicate": 400,
  "pass_accept": 400,
  "pass_exit_required": 400,
  "offline_single_queued": 200,
  "offline_pass_queued": 200
}
```

SHA-256:

```json
{
  "samples.jsonl": "1427f4a21ec01eea37f338a09773546d82eb6176ec0b80f1573be3a54fbfcf21",
  "warmups.jsonl": "6af1d5306b7017b19eec339e67e138721bab54c87196cde0387c2bbb896d71fe",
  "health.jsonl": "5b2b4ec4b8d408145b8b449aa149a70eeed433d290f1d3c8316bc370b7ee9d86",
  "fixtures.json": "7f7bd8b1a5aba5d5ceca441986a897f46ce3144f8548443f52e4099687740754",
  "correctness.json": "910bc79065dd0f13bcf7844aca25fc683dda238cd618ed527b6d701bea09e478"
}
```

Combined result: **profile_meets_target**
