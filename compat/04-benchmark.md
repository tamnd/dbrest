# Benchmark report: dbrest vs PostgREST v14

This document records throughput and latency measurements comparing dbrest to
PostgREST v14.2.3, both backed by the same PostgreSQL 18-alpine instance.
Measurements are taken with the compat test suite (`go test ./compat/ -v -run
TestPerformanceComparison,TestConcurrentThroughput`) and with wrk for raw HTTP
numbers. The 5x throughput target is the stated project goal.

## Setup

| Component | Version |
|-----------|---------|
| dbrest    | HEAD (compat-full branch) |
| PostgREST | v14.2.3 |
| PostgreSQL | 18-alpine |
| Go        | 1.26-alpine (dbrest binary) |
| Host      | Apple M1, 16 GB, macOS 14 |

Both stacks run under `podman compose` on the same host with separate PG
instances (PostgREST on port 5433, dbrest on port 5434) to avoid cross-
contamination of connection pools. The seed schema (todos / persons /
assignments) is identical; both use the same `web_anon` role and `api` schema.

Connection pool: both servers configured to 10 connections, matching
PostgREST's default.

## Architecture advantages

### Session setup: one batch vs three round-trips

PostgREST performs three or more sequential statements at the start of every
transaction:

1. `SET LOCAL ROLE <role>`
2. `SET LOCAL search_path TO api`
3. `SELECT set_config(...)` x5 (sometimes as individual calls)

dbrest merges all of these into a single `pgx.Batch` with `SendBatch`, so the
pool sees ONE round-trip instead of three. On a loopback socket this is small
(< 0.1 ms), but at any real DB latency (1 ms+ RTT) it halves the per-request
overhead.

### Prepared statement caching

dbrest enables `pgx.QueryExecModeCacheDescribe` on every pool connection. The
server parses each distinct SQL string once per connection and stores the type
descriptor. Repeated reads and writes within the same connection lifetime need
no re-parse. PostgREST also uses prepared statements, but pgx's implementation
has lower per-statement overhead than the Haskell PostgreSQL driver.

### Go concurrency vs PostgREST's connection-per-worker model

PostgREST allocates one PG connection per in-flight request. dbrest uses
`pgxpool` with non-blocking acquire: a goroutine blocks only when all 10
connections are busy, then picks up again as soon as one is released. This
means dbrest handles bursts without connection pressure.

## Sequential throughput (single goroutine, localhost PG)

Measurement: `TestPerformanceComparison` in `compat/compat_test.go` — 50 warmup
requests then 500 timed requests per path, single goroutine, results recorded
after CI baseline.

| Scenario | PostgREST req/s | dbrest req/s | Ratio |
|----------|----------------|--------------|-------|
| GET /todos | — | — | — |
| GET /todos?select=id,task | — | — | — |
| GET /todos (count=exact) | — | — | — |
| GET /persons (embed) | — | — | — |

*(Values populated after first live run; see "Running the benchmarks" below.)*

## Concurrent throughput (20 goroutines, 1000 total requests)

Measurement: `TestConcurrentThroughput` — 20 concurrent goroutines racing 1000
GET /todos requests to each server. This is the scenario where Go's scheduler
matters most.

| Scenario | PostgREST req/s | dbrest req/s | Ratio |
|----------|----------------|--------------|-------|
| GET /todos (20 concurrent) | — | — | — |

## wrk raw HTTP throughput

```
# PostgREST
wrk -t4 -c20 -d10s http://localhost:3000/todos

# dbrest
wrk -t4 -c20 -d10s http://localhost:3001/todos
```

Results recorded after first docker-compose run.

## Latency breakdown (p50 / p95 / p99)

To be filled in with `wrk --latency` or `hey` output.

## Known bottlenecks

1. **Introspection cache**: dbrest does not yet cache the schema model across
   requests. Each first request after a `NOTIFY pgrst` does a full catalog scan.
   PostgREST caches aggressively. This is spec 04/capability-grading work, not
   blocking compat.

2. **COPY path for bulk insert**: not implemented. Bulk inserts use VALUES lists
   like PostgREST. A COPY-based path could improve bulk throughput significantly.

3. **Connection pool starvation at > 10 concurrent**: both servers share the
   same 10-connection cap; concurrency above that queues. Raising pool_max_conns
   helps both equally.

## Running the benchmarks

Start both stacks:

```
podman compose -f docker/postgrest/compose.yaml up -d
podman compose -f docker/dbrest/compose.yaml up -d
```

Wait for health:

```
curl -s http://localhost:3000/ | head -c 40
curl -s http://localhost:3001/ | head -c 40
```

Run the compat + benchmark suite:

```
go test ./compat/ -v -timeout 300s -run TestPerformanceComparison,TestConcurrentThroughput
```

Run all conformance tests:

```
go test ./compat/ -v -timeout 120s -run TestCompatibility
```

Full summary:

```
go test ./compat/ -v -timeout 120s
```
