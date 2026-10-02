# Durable workflow benchmark measurements

The storage and matching benchmarks passed against SDK commit
`54d350057da1d7fa0e733b0afd81efd3558f7dc8`. Measurements were recorded at
2026-10-02 19:31:14 UTC (October 3 in Asia/Kolkata). These measurements describe
local storage behavior; live chat latency under workflow load remains unmeasured.

## Method

The machine ran Linux/amd64 on an AMD Ryzen 9 5900HX with other workloads active.
Go was `go1.27.1-X:nodwarf5`. Both commands used `GOMAXPROCS=1`, one build worker,
and one test worker. SQLite benchmarks ran 20 fixed iterations per case; the
memory ingestion benchmarks ran 200. Results are arithmetic means from one run,
not latency percentiles or a regression threshold.

From the repository root:

```bash
GOMAXPROCS=1 go test -p 1 -parallel 1 ./pkg/runtime \
  -run '^$' \
  -bench '^BenchmarkRunEventWaitSQLite(PublishResolve|IdleDiscovery|Fanout100)$' \
  -benchmem -benchtime=20x -count=1 -cpu=1 -timeout=10m

GOMAXPROCS=1 go test -p 1 -parallel 1 ./pkg/runtime \
  -run '^$' \
  -bench '^BenchmarkMemoryRunEventPublishIndexedWaits$' \
  -benchmem -benchtime=200x -count=1 -cpu=1 -timeout=2m
```

Fixture creation, migrations, and wait registration are excluded from the timed
portion. The fixtures contain actual dormant waits backed by SQLite transactions.
Fixed iteration counts bound fixture growth and avoid adaptive benchmark setup
repeatedly growing the stored workload. The SQLite command took 157.089 seconds
including setup; the memory command took 1.721 seconds.

## SQLite results

| Operation | Dormant waits | Mean per operation | Allocated bytes per operation | Allocations per operation |
| --- | ---: | ---: | ---: | ---: |
| Discover due scopes with no ready work | 100 | 20.123 microseconds | 4,560 | 32 |
| Discover due scopes with no ready work | 10,000 | 21.616 microseconds | 4,560 | 32 |
| Publish one event, match its wait, and queue the continuation | 100 | 3.635951 milliseconds | 395,865 | 2,086 |
| Publish one event, match its wait, and queue the continuation | 10,000 | 5.327134 milliseconds | 395,784 | 2,086 |
| Publish one event and resolve 100 matching waits | 100 unrelated dormant waits | 311.706707 milliseconds per 100 resolutions | 8,476,962 | 105,762 |

Increasing dormant waits 100-fold increased mean idle discovery time by 7.4%
and mean event publication plus match/queue time by 46.5%, with unchanged
allocation counts. The fanout case processed 100 matching waits in four batches
of 25; its timing is the total for all four batches.

Publication and resolution include event validation/digest generation, durable
publication, notification matching, claims, receipt consumption, audit and
checkpoint updates, and the canonical Run transition to queued. Queuing a
continuation does not execute the resumed Agent or deliver its report.

## Memory ingestion results

| Waiting workflows | Matching fanout | Mean ingestion time | Allocated bytes per operation | Allocations per operation |
| ---: | --- | ---: | ---: | ---: |
| 1 | No | 44.814 microseconds | 29,116 | 106 |
| 10,000 | No | 95.209 microseconds | 29,113 | 106 |
| 10,000 | Yes | 25.832 microseconds | 29,099 | 105 |

This benchmark includes receipt construction and digest generation. It leaves
notifications unprocessed and accumulates stored events, so the fanout case
measures ingestion only. The timings do not establish how long 10,000
continuations take to resume. The faster fanout result is not evidence that
larger workloads improve performance.

## Scope and evidence

The measurements support indexed dormant-wait scaling. They do not establish a
before/after platform improvement or guarantee no platform slowdown. Provider
authentication, host timer delay, model/tool execution, report delivery,
retention cleanup, production PostgreSQL contention, HTTP/chat latency, and
p95/p99 latency are outside these benchmarks.

The [benchmark output](durable-workflows-20261003.txt), with trailing whitespace removed, and
[structured measurements](durable-workflows-20261003.json) preserve the recorded
results, exact commands, environment, and limitations.
