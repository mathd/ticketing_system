# TKT-348 probe results

Executed by the orchestrator on 2026-09-27 against base commit `96a93288007eb331adb3bcbfae236cc08d2cc354`, with the adjacent uncommitted investigation files. Production source was unchanged.

```sh
bash docs/evidence/TKT-348/run.sh /tmp/tkt348-r22yx5jm/probe-observed.json
```

Exit status: `0`. Output: `probe output: /tmp/tkt348-r22yx5jm/probe-observed.json`. The unmodified output is preserved in [observed.json](observed.json).

All six independent schemas produced this sequence:

| Step | Observed result |
| --- | --- |
| Pool of five filled by operational hold | Public request refused with HTTP 409, insufficient capacity |
| One operational unit released | Source remains held with quantity four |
| Public request A completes before B starts | HTTP 201, one buyer unit held |
| Later request B | HTTP 409, insufficient capacity |
| Database re-read | Four operational units plus one buyer unit; confirmed zero; capacity five; no target cut |

The capacity invariant held in every repetition. This is a forced admissible ordering, not a random race measurement or a test of a waitlist implementation. It proves that public admission can consume the released unit before a later ordinary hold request. Other release sources were read in code, not executed by this probe.

The runner removed its disposable container. No running application database was changed. Static Go build, shell syntax and diff checks passed. Full repository gate and cross-model artifact review were pending when this result was recorded.
