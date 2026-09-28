# TKT-492 analysis

Profile: `tkt492-local-scanner-v1`

## Attempt 01

Result: **incomplete**
Generator: valid. Correctness: inconclusive. Latency: inconclusive.

Measured samples: 2000. Maximum: 77.19999992847443 ms. Samples at or above 500 ms: 0.
Latency p50/p95/p99: 40.300000071525574/52.60000002384186/61.5 ms.
Scheduler lag p99/max: 0.30009758472442627/1.6000487804412842 ms.
Validity: missed=0, overlap=null, unknown transport=0.

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
  "samples.jsonl": "bbe2d3b2fb2ab321bc01799624e6d36a813bea5d99c37f581c0e8eef7f695753",
  "warmups.jsonl": "8e336863d12bbac363babdb3f48124bede721921991c0f6d4b4e6bca15f696c1",
  "health.jsonl": "b74262479ea212f3a2a775b9560f9ca3cfa21b77dabfa082743a4525a1cfca6d",
  "fixtures.json": "7f7bd8b1a5aba5d5ceca441986a897f46ce3144f8548443f52e4099687740754",
  "correctness.json": "a0400e346b7bbb38a03c240bea0ce5f68478e1eb0622fdbdecac4c4ab565c23e"
}
```

Combined result: **incomplete**
