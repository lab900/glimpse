# glimpse

[![ci](https://github.com/lab900/glimpse/actions/workflows/ci.yml/badge.svg)](https://github.com/lab900/glimpse/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/lab900/glimpse)](https://github.com/lab900/glimpse/releases)

A small trace collector in one Go binary. Your app sends OpenTelemetry traces
to it. glimpse stores them in SQLite and tells you what happened behind a
request: slow endpoints, N+1 queries, slow SQL, errors, and execution plans.

It is made for two readers:

- **A person**, through the built-in UI.
- **An AI agent**, through a plain-text digest of one trace and a small JSON API.

![Overview: endpoints, N+1 suspects, slow queries, errors](docs/screenshots/overview.png)

![Trace page: waterfall across two services, with the queries of the trace](docs/screenshots/trace.png)

## Contents

- [Quick start](#quick-start)
- [Install](#install)
- [Send traces from a Java app](#send-traces-from-a-java-app)
- [What you get](#what-you-get)
- [Use it with an AI agent](#use-it-with-an-ai-agent)
- [One trace per e2e test](#one-trace-per-e2e-test)
- [Execution plans](#execution-plans)
- [Configuration](#configuration)
- [Security](#security)
- [Development](#development)

## Quick start

```sh
gh release download --repo lab900/glimpse --pattern '*darwin-arm64*'   # or linux-amd64, ...
tar -xzf glimpse-darwin-arm64.tar.gz
./glimpse-darwin-arm64
```

Start your app with the OpenTelemetry Java agent (see
[below](#send-traces-from-a-java-app)), make some requests, and open
http://localhost:4318/.

glimpse listens on port 4318, the OTLP/HTTP default. An OpenTelemetry agent on
the same host needs no endpoint configuration.

## Install

### Docker

No image is published. Build it from the `Dockerfile`, which cross-compiles for
linux/amd64 and linux/arm64. The image runs as a non-root user and keeps its
database in the `/data` volume.

```sh
docker build --build-arg VERSION=v0.2.0 -t glimpse:0.2.0 https://github.com/lab900/glimpse.git#v0.2.0
```

```yaml
services:
  glimpse:
    image: glimpse:0.2.0
    ports: ["127.0.0.1:4318:4318"]
    volumes: ["glimpse-data:/data"]
    # environment:
    #   GLIMPSE_MSSQL_DSN: sqlserver://user:password@db:1433         # optional
    #   GLIMPSE_POSTGRES_DSN: postgres://user:password@db:5432/app   # optional
volumes:
  glimpse-data:
```

Other containers in the same compose network reach it at `http://glimpse:4318`.

### Binary

Each release has binaries for linux, macOS and Windows (amd64 and arm64), named
`glimpse-<os>-<arch>.tar.gz` (`.zip` on Windows), plus `checksums.txt`.

```sh
gh release download --repo lab900/glimpse --pattern '*linux-amd64*' --pattern checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar -xzf glimpse-linux-amd64.tar.gz
./glimpse-linux-amd64
```

### From source

```sh
go build -o glimpse ./cmd/server
./glimpse
```

Go 1.25 is the only requirement. There is no CGO, so `GOOS` and `GOARCH` are
enough to cross-compile.

## Send traces from a Java app

Get the [OpenTelemetry Java agent](https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases)
with `make agent`, or from Maven Central:
`io.opentelemetry.javaagent:opentelemetry-javaagent:2.30.0`.

```sh
java -javaagent:opentelemetry-javaagent.jar \
  -Dotel.exporter.otlp.protocol=http/protobuf \
  -Dotel.exporter.otlp.endpoint=http://localhost:4318 \
  -Dotel.metrics.exporter=none \
  -Dotel.logs.exporter=none \
  -Dotel.service.name=my-app \
  -jar my-app.jar
```

In a compose stack, use environment variables:

```yaml
environment:
  JAVA_TOOL_OPTIONS: -javaagent:/otel/opentelemetry-javaagent.jar
  OTEL_SERVICE_NAME: my-app
  OTEL_EXPORTER_OTLP_ENDPOINT: http://glimpse:4318
  OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf
  OTEL_METRICS_EXPORTER: none
  OTEL_LOGS_EXPORTER: none
```

Do not run it beside another OpenTelemetry-based agent (for example
Application Insights 3.x) in the same JVM.

Any other OpenTelemetry SDK or agent works too, if it exports OTLP over HTTP
(protobuf or JSON, gzip accepted) to `POST /v1/traces`.

<details>
<summary>IntelliJ run configuration</summary>

1. Open **Run → Edit Configurations…** and select your Spring Boot configuration.
2. Add this to the **VM options** field. Keep your existing options. These go
   outside any `-Dspring-boot.run.jvmArguments="..."` string, which IntelliJ
   ignores when it starts the JVM directly.

```
-javaagent:/path/to/opentelemetry-javaagent.jar -Dotel.exporter.otlp.protocol=http/protobuf -Dotel.exporter.otlp.endpoint=http://localhost:4318 -Dotel.metrics.exporter=none -Dotel.logs.exporter=none -Dotel.service.name=my-app
```

3. Apply and start the app. An `otel.javaagent` line near the top of the
   console confirms that the agent is active.

</details>

## What you get

Everything is on one port.

| Path | What it is |
| --- | --- |
| `/` | UI: endpoints, traces, N+1 suspects, slowest queries, recent errors |
| `/trace/{traceId}` | Trace page: waterfall, span details, queries. The URL is shareable. |
| `POST /v1/traces` | OTLP/HTTP ingest |
| `GET /api/endpoints` | Calls, errors, avg, p50, p95 and max per endpoint |
| `GET /api/insights` | N+1 suspects, slow SQL, error spans |
| `GET /api/traces` | Recent traces, optionally for one endpoint |
| `GET /api/traces/{traceId}` | All spans of a trace, with attributes |
| `GET /api/traces/{traceId}/digest` | One trace as plain text, made for an LLM |
| `POST /api/queryplan` | Execution plan of a captured statement (SQL Server or Postgres) |
| `GET /api/stats` | Version, span counts per service, database size, plan engines |
| `DELETE /api/spans` | Deletes all stored spans |
| `/docs`, `/openapi.json` | API documentation |

The list endpoints take `window_minutes` (1 to 10080, default 60).

How to read the insights:

- **N+1 suspect:** one trace repeats the same statement 5 times or more.
- **Slow SQL:** statements sorted by average duration, with the slowest trace.
- **Errors:** spans with an error status, with the exception type and message.

The digest of a trace looks like this:

```
TRACE 34a069e3fab8c3bfc5e740e61572b4e3 — POST /orders
service: inventory, orders-api | start: 2026-11-23T19:55:02Z | total: 140ms | spans: 9 | errors: 0

SPAN TREE (indent = child, @+ = offset from trace start)
[server] orders-api: POST /orders 140ms @+0.0ms http=201
  [client] orders-api: SELECT shop.product 5.0ms @+4.0ms sql="select p1_0.id,... where p1_0.id=?"
  [producer] orders-api: order-events publish 4.0ms @+30ms dest=order-events
    [consumer] inventory: order-events process 80ms @+52ms dest=order-events
      [client] inventory: SELECT shop.stock 6.0ms @+58ms sql="select ... where s1_0.product_id=?"
      ...

REPEATED QUERIES (possible N+1)
3x (19ms total): select s1_0.product_id,s1_0.quantity from stock s1_0 where s1_0.product_id=?
```

## Use it with an AI agent

The fastest path: press **Copy digest** on a trace page and paste it into the
chat. Or give the agent a trace id and let it call the API:

```sh
curl -s "localhost:4318/api/traces/<traceId>/digest"       # one trace, plain text
curl -s "localhost:4318/api/insights?window_minutes=60"    # N+1 suspects, slow SQL, errors
curl -s "localhost:4318/api/endpoints?window_minutes=60"   # latency and errors per endpoint
curl -s "localhost:4318/api/traces?endpoint=GET%20/orders" # recent traces of one endpoint
```

For Claude Code there is a ready skill in [`skill/SKILL.md`](skill/SKILL.md).
It teaches Claude to recognize trace ids, fetch the digest, and read the
insights and plans. Install it with:

```sh
mkdir -p ~/.claude/skills/glimpse
cp skill/SKILL.md ~/.claude/skills/glimpse/SKILL.md
```

Set `GLIMPSE_URL` if glimpse does not run on `http://localhost:4318`.

## One trace per e2e test

Send a W3C `traceparent` header on the API calls of a test, with one fixed
trace id per test. The backend agent continues that trace, so all backend work
of the test is in one trace. When the test fails, attach
`/api/traces/<traceId>/digest` to the report.

```ts
import crypto from 'node:crypto';

const traceId = crypto.randomBytes(16).toString('hex');
await page.route('**/api/**', route => {
  const spanId = crypto.randomBytes(8).toString('hex');
  route.continue({
    headers: { ...route.request().headers(), traceparent: `00-${traceId}-${spanId}-01` },
  });
});
// on failure: fetch http://localhost:4318/api/traces/<traceId>/digest
```

Each backend request is a separate root span in that trace. The digest and the
trace page show all of them.

## Execution plans

Slow-query and N+1 entries get an **Explain** button when a database is
configured. The API is `POST /api/queryplan` with `{"statement": "..."}`.
glimpse never executes captured SQL.

### SQL Server

Set `GLIMPSE_MSSQL_DSN`, for example `sqlserver://sa:PASSWORD@127.0.0.1:1433`.
glimpse reads the plan cache DMVs (read-only, needs `VIEW SERVER STATE`). It
returns missing-index suggestions with ready-to-run DDL, plan warnings,
operator counts, execution statistics, and the plan XML.

### Postgres (first version)

Set `GLIMPSE_POSTGRES_DSN`, for example
`postgres://user:password@127.0.0.1:5432/app`. PostgreSQL 16 or newer.

What it does for a captured statement:

- It converts the `?` placeholders to `$1, $2, ...` and runs
  `EXPLAIN (GENERIC_PLAN, FORMAT JSON)`. This plans the statement and does not
  run it. glimpse never uses `ANALYZE`.
- It runs in a read-only transaction with `statement_timeout = 3s`. The
  transaction is always rolled back.
- It accepts one statement that starts with `SELECT`, `WITH`, `INSERT`,
  `UPDATE` or `DELETE`.
- It counts the node types in `operators`. It adds a warning for a `Seq Scan`
  with a filter on a table of 10000 or more estimated rows. The warning only
  says that an index may help.
- If `pg_stat_statements` has the statement, it fills `execution_count`,
  `avg_elapsed_ms` and `avg_logical_reads` (shared buffer blocks per call).
  Otherwise these stay empty.

The response has `engine: "postgres"` and `plan_json` (the raw plan).

Limits:

- No `CREATE INDEX` DDL. Postgres gives no missing-index hints, so
  `missing_indexes` is always empty.
- A generic plan has no parameter values. It can differ from the plan the
  server picks for real values. A parameter whose type Postgres cannot infer
  (for example `$1 IS NULL`) makes the statement fail to plan.
- A `?` that is a Postgres JSON operator (`?`, `?|`, `?&`) is converted to a
  placeholder too. Such statements fail to plan. Dollar-quoted strings and
  `E''` strings are not recognized.
- The table size lookup uses the server `search_path`.

Privileges: the role needs `CONNECT` and `SELECT` on the tables in the
statement. To see `pg_stat_statements` rows of other roles it needs
`pg_read_all_stats`.

If both DSNs are set, send `db_system` in the request (`postgresql` picks
Postgres). Without it, SQL Server is used.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `HOST` | `127.0.0.1` | Listen address. `0.0.0.0` listens on all interfaces. |
| `PORT` | `4318` | Listen port. |
| `DB_PATH` | `./data/glimpse.db` | SQLite file. |
| `RETENTION_HOURS` | `24` | Older spans are purged at start and every 10 minutes. |
| `GLIMPSE_MSSQL_DSN` | unset | SQL Server connection string. Enables SQL Server plans. |
| `GLIMPSE_POSTGRES_DSN` | unset | `postgres://` URL. Enables Postgres plans. |

All variables can also be in a `.env` file next to the binary (see
`.env.example`). Real environment variables win over `.env`.

The binary migrates its own database at start.

## Security

glimpse has no authentication, and `DELETE /api/spans` deletes all data.
Captured SQL statements and span attributes can contain sensitive data.

- The binary binds to `127.0.0.1` by default.
- The Docker image sets `HOST=0.0.0.0`, because a container needs it. Publish
  the port on loopback only: `127.0.0.1:4318:4318`.
- Use glimpse for development and test stacks. Do not expose it on a network
  you do not trust.

## Development

```sh
make tools      # install sqlc and goose
make run        # generate the db package and start the server
make test       # go test ./...
make build      # build ./glimpse with the version from git
make agent      # download the pinned OpenTelemetry Java agent
```

The stack is Huma (HTTP and OpenAPI), sqlc (typed queries), goose (migrations)
and SQLite through `modernc.org/sqlite`. See [AGENTS.md](AGENTS.md) for the
layout and the rules of the code base.

A release is one tag: `git tag v1.2.3 && git push origin v1.2.3`. The release
workflow builds the binaries and creates the GitHub release.

## Licence

MIT, see [LICENSE](LICENSE).
