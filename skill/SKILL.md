---
name: glimpse
description: Read runtime trace data from the user's local glimpse server (tiny Digma-style OTLP trace collector at localhost:4318). Use when the user pastes a trace id (32-char hex), a glimpse trace URL (/trace/<id>), asks about request performance, N+1 queries, slow endpoints, or runtime errors in their app, or mentions glimpse.
---

# glimpse: runtime trace insights

glimpse is a local trace collector. The user's app sends OpenTelemetry traces to
it. You can query it with curl. Base URL: `$GLIMPSE_URL` if set, otherwise
`http://localhost:4318` (the examples below write the default; swap in
`$GLIMPSE_URL` when it is set). If the connection fails, ask the user whether
glimpse runs and on which host and port. glimpse binds to 127.0.0.1 by default.

## When the user gives you a trace id

A trace id is 32 hex chars (e.g. `6444aa0921a50233eb51ef29f5413146`), sometimes
inside a URL like `http://localhost:4318/trace/<id>`. Fetch the digest first —
it is plain text made for you:

```sh
curl -s http://localhost:4318/api/traces/<id>/digest
```

It contains: the root operation, total duration, the span tree with per-span
durations and offsets (each span is labelled with its service when the trace
spans more than one service), SQL statements, HTTP status, error messages, and repeated
queries (possible N+1). For raw span attributes, fetch the JSON instead:
`curl -s http://localhost:4318/api/traces/<id>` .

## Wider questions ("why is my app slow?", "any N+1s?")

```sh
# per-endpoint latency (avg/p50/p95/max), call counts, error counts
curl -s "http://localhost:4318/api/endpoints?window_minutes=60"

# N+1 suspects, slowest SQL, recent errors
curl -s "http://localhost:4318/api/insights?window_minutes=60"

# recent traces for one endpoint (name = span name, e.g. "GET /owners/{ownerId}")
curl -s "http://localhost:4318/api/traces?endpoint=GET%20%2Fvets&limit=10"
```

`window_minutes` accepts 1–10080. Full OpenAPI spec: `/openapi.json`.

## Server state

`curl -s http://localhost:4318/api/stats` returns the glimpse `version`,
`db_bytes` (database size), `spans_by_service` (span count per service name),
`mssql_plans` and `postgres_plans`. Use `spans_by_service` to check that an app sends data, and
to find the exact service names.

## Execution plans (MSSQL or Postgres)

If `/api/stats` reports `"mssql_plans": true` or `"postgres_plans": true`, you
can fetch the execution plan for any captured statement:

```sh
curl -s -X POST http://localhost:4318/api/queryplan \
  -H "Content-Type: application/json" \
  -d '{"statement": "<db.statement from a span, verbatim>", "db_system": "<db.system of the span>"}'
```

`db_system` is optional. It matters only when both engines are configured:
`postgresql` picks Postgres, anything else picks MSSQL. The response has
`engine` (`mssql` or `postgres`).

- MSSQL: reads the plan cache. Returns missing-index suggestions with
  ready-to-run `CREATE INDEX` DDL, plan warnings, notable operators, execution
  counts and avg logical reads. `plan_xml` holds the showplan XML. Prefer the
  plan's own index suggestion over guessing.
- Postgres (PostgreSQL 16+): runs `EXPLAIN (GENERIC_PLAN, FORMAT JSON)`, which
  does not execute the statement. `missing_indexes` is always empty: Postgres
  gives no index suggestions. `warnings` flags a filtered Seq Scan on a large
  table, as a hint only. `operators` counts the node types. `plan_json` holds
  the raw plan. Stats (`execution_count`, `avg_elapsed_ms`,
  `avg_logical_reads`) come from `pg_stat_statements` and may be empty. A
  generic plan has no parameter values, so check the hint against the code
  before you recommend an index.
- `found: false` comes with a `reason`.

## How to work with the data

- Durations are milliseconds in JSON; the digest uses ms/s suffixes.
- Connect findings to code: `http.route` maps to a controller method, SQL
  statements map to repositories/queries. Look up the code and point at it.
- N+1 signature: one server span with many child spans repeating the same SQL.
  The fix is usually a join fetch / batch load at the code location that issues
  the repeated query.
- If an endpoint looks slow, compare p50 vs p95 (spiky vs uniformly slow) and
  open one slow trace (`/api/traces?endpoint=...`, then the digest of the
  slowest one).
