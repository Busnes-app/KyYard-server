# Soak fixture

`cmd/soak` drives the storage path at the capacity targets in
[retention-policy.md](retention-policy.md) and checks that the bounds that document promises
actually hold under sustained load. M4's gate asks for "a sustained fixture run" that proves
"configured database/storage bounds and cleanup"; this is that run.

## Running it

The full run, as the policy specifies, is 24 hours on SQLite at the targets:

```
go run ./cmd/soak -endpoints 25 -containers 100 -duration 24h
```

It prints progress every five minutes, a report at the end, and exits non-zero if any bound
failed. A short run at the same targets is a useful smoke check before committing the day:

```
go run ./cmd/soak -duration 5m -cadence 10s -retention-interval 10s -report 1m
```

A cadence below `store.SampleCadence` (50 s) does not store more rows: `RecordSamples` keeps at
most one row per container per cadence and drops the rest, which is the bound it exists to
enforce. A fast short run therefore offers many samples and stores few, and the report prints
both numbers so the difference cannot be mistaken for throughput.

Every knob has a flag: `-endpoints`, `-containers`, `-cadence`, `-retention-interval`,
`-budget`, `-dir`, `-report`, `-quiet`. With no `-dir` it works in a temporary directory and
removes it afterwards.

## What it asserts

| Question | How |
|---|---|
| Do stored rows stay under the per-endpoint ceiling? | counts rows per endpoint against `SampleCeiling` |
| Does retention keep up with the writers? | the oldest surviving sample must sit inside the raw window, the oldest summary inside the roll-up window |
| Does the disk budget engage, and can it be left? | records every pressure level seen; exceeding the budget without ever returning to normal is a failure |
| Are refusals recorded and never leaked? | a read-only member of a second organization attempts a mutation against a target identifier it has never used, for the whole run, while the first organization keeps writing |
| Can an administrator still tidy up afterwards? | removes that member at the end |
| Do the list screens stay fast while all of it runs? | times `ListEndpoints` plus `LatestSamples` once a second and reports p95 against the 500 ms target. A read that fails is timed and counted too, never dropped: read failures under load are the degradation the gate exists to catch |
| Was storage exercised at all? | counts rows actually in the database, not samples offered. A run that stored nothing satisfies every other bound here, so it is a failure |
| Did the budget hold? | final usage must be at or under the budget, and if peak usage passed it, the ceiling must have refused at least one write. The pressure level returning to normal proves nothing on its own, since any prune pass clears it |
| Could a read-only member of another organization reach a real endpoint? | a second probe targets a real, approved endpoint in the writers' organization and accepts only `ErrForbidden`. "Not found" would mean the boundary had become a lookup oracle |

## What it does not cover

Stated here rather than assumed, and repeated in the report the command prints:

- **Log-client memory.** Bounded buffers for slow log and terminal clients arrive with M5;
  there is no log transport to attach to yet.
- **The per-actor denial budget and the per-organization audit ceiling.** Both are still
  `proposed` in the retention policy. The fixture exercises the deny path and counts refusals,
  but cannot assert a budget that is not implemented.
- **The agent socket.** The fixture writes through the store. The gate's question is about
  database and storage bounds; a websocket per endpoint would measure the transport instead.
  The socket path has its own tests in `internal/api`.

## Keeping it honest

`cmd/soak/soak_test.go` runs the same driver for a few seconds with small targets on every
`make ci`, and asserts the run actually wrote samples, exercised refusals, read the list
screens, and named its own gaps. That proves the harness still works. It does not prove the
day-long bounds, which is what the 24-hour command is for.
