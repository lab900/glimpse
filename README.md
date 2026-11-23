# glimpse

A tiny Digma-style trace collector in one Go binary. It receives OTLP/HTTP
traces from an OpenTelemetry agent, stores them in SQLite, and shows code
insights: slow endpoints, N+1 queries, slow SQL, and errors.

## Run

### Option A: download a release (no Go needed)

Each tagged release has binaries for linux, darwin and windows (amd64 and
arm64), named `glimpse-<os>-<arch>.tar.gz` (`.zip` on windows), plus
`checksums.txt` (sha256). From the
[releases page](https://github.com/lab900-luis-santos/glimpse/releases), or
with the GitHub CLI:

```sh
gh release download --repo lab900-luis-santos/glimpse --pattern '*linux-amd64*'
gh release download --repo lab900-luis-santos/glimpse --pattern checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar -xzf glimpse-linux-amd64.tar.gz
./glimpse-linux-amd64
```

Note: v0.1.1 only has `glimpse-darwin-arm64`. The other platforms and the
Docker image exist from the next tagged release on. Until then, build from
source.

### Option B: Docker

The image is `ghcr.io/lab900-luis-santos/glimpse` (linux/amd64 and linux/arm64,
tags `latest` and the version). It runs as a non-root user and keeps its
database in the `/data` volume.

```yaml
services:
  glimpse:
    image: ghcr.io/lab900-luis-santos/glimpse:latest
    ports: ["127.0.0.1:4318:4318"]
    volumes: ["glimpse-data:/data"]
    # environment:
    #   GLIMPSE_MSSQL_DSN: sqlserver://user:password@db:1433   # optional
volumes:
  glimpse-data:
```

Other containers in the same compose network reach it at `http://glimpse:4318`.

glimpse has no authentication, and `DELETE /api/spans` wipes all data. So it
binds to `127.0.0.1` by default. The image sets `HOST=0.0.0.0`, which is for
containers and trusted networks only. Publish the port on loopback as above.

### Option C: build from source

```sh
make build      # or: go build -o glimpse ./cmd/server
./glimpse
```

`make build` runs `sqlc generate` first (`make tools` installs it). The `db/`
package is committed, so plain `go build ./cmd/server` needs no sqlc.

The binary migrates its own database on startup. Everything runs on one port:

- UI:          http://localhost:4318/
- Trace page:  http://localhost:4318/trace/{traceId} (shareable, collapsible span tree)
- OTLP ingest: POST http://localhost:4318/v1/traces (protobuf or JSON, gzip OK)
- JSON API:    /api/endpoints, /api/insights, /api/traces, /api/traces/{traceId}, /api/stats
- AI digest:   /api/traces/{traceId}/digest (plain text, made for LLMs)
- Plans:       POST /api/queryplan (MSSQL only, see below)
- Wipe:        DELETE /api/spans (deletes all stored spans)
- API docs:    http://localhost:4318/docs, spec at /openapi.json

Environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `HOST` | `127.0.0.1` | Listen address. `0.0.0.0` listens on all interfaces. |
| `PORT` | `4318` | Listen port. |
| `DB_PATH` | `./data/glimpse.db` | SQLite file. |
| `RETENTION_HOURS` | `24` | Spans older than this are purged (also once at startup). |
| `GLIMPSE_MSSQL_DSN` | unset | SQL Server connection string, enables execution plans. |

All variables can also live in a `.env` file next to the binary (see
`.env.example`). Real environment variables win over `.env`, and `.env` is
gitignored.

### MSSQL execution plans (optional)

Set `GLIMPSE_MSSQL_DSN` to a SQL Server connection string, e.g.
`sqlserver://sa:PASSWORD@127.0.0.1:1433`. Slow-query and N+1 entries then get
an **explain** button: glimpse reads the plan cache DMVs (read-only, needs
`VIEW SERVER STATE`) and shows missing-index suggestions with ready-to-run
DDL, plan warnings, operator counts, and real execution stats. The API is
`POST /api/queryplan` with `{"statement": "..."}`. glimpse never executes
captured SQL against your database.

## Attach a Spring Boot app

Get the [OpenTelemetry Java agent](https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases)
with `make agent`, or from Maven Central:
`io.opentelemetry.javaagent:opentelemetry-javaagent:2.30.0`. The jar is not in
git. Start your app with:

```sh
java -javaagent:opentelemetry-javaagent.jar \
  -Dotel.exporter.otlp.protocol=http/protobuf \
  -Dotel.exporter.otlp.endpoint=http://localhost:4318 \
  -Dotel.metrics.exporter=none \
  -Dotel.logs.exporter=none \
  -Dotel.service.name=my-app \
  -jar my-app.jar
```

In a compose stack, use environment variables instead:

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

Make some requests, then open the UI.

### IntelliJ run configuration

1. Open **Run → Edit Configurations…** and select your Spring Boot configuration.
2. Add this to the **VM options** field (keep your existing options; these go
   outside any `-Dspring-boot.run.jvmArguments="..."` string, which IntelliJ
   ignores when it starts the JVM directly):

```
-javaagent:/path/to/opentelemetry-javaagent.jar -Dotel.exporter.otlp.protocol=http/protobuf -Dotel.exporter.otlp.endpoint=http://localhost:4318 -Dotel.metrics.exporter=none -Dotel.logs.exporter=none -Dotel.service.name=my-app
```

3. Apply and start the app. A `otel.javaagent` line near the top of the console
   confirms the agent is active.

## One trace per e2e test

Send a W3C `traceparent` header (`00-<32 hex trace id>-<16 hex span id>-01`)
on the API calls of a test. Use a fixed trace id per test. The backend agent
continues that trace. When the test fails, fetch
`/api/traces/<traceId>/digest`. Each backend request is a separate root span,
so one trace holds several root spans.

Playwright example:

```ts
import crypto from 'node:crypto';

const traceId = crypto.randomBytes(16).toString('hex');
await page.route('**/api/**', route => {
  const spanId = crypto.randomBytes(8).toString('hex');
  route.continue({
    headers: { ...route.request().headers(), traceparent: `00-${traceId}-${spanId}-01` },
  });
});
// on failure: curl http://localhost:4318/api/traces/<traceId>/digest
```

## Reading the output

The digest of a trace shows the root operation, total duration, the span tree
with durations, SQL statements, HTTP status, errors, and repeated queries. If
the trace has more than one service, each span carries its service name.

`/api/insights` shows three things: N+1 suspects (the same statement 5 or more
times in one trace), slow SQL, and error spans. `/api/stats` shows the
version, span counts per service, and database size.

Execution plans work for SQL Server only. There are no Postgres plans.

## Give an AI insight into your app

The JSON API is made for this. Examples:

```sh
curl -s "localhost:4318/api/endpoints?window_minutes=60"   # latency + error stats per endpoint
curl -s "localhost:4318/api/insights?window_minutes=60"    # N+1 suspects, slow SQL, errors
curl -s "localhost:4318/api/traces?endpoint=GET%20/vets"   # recent traces for one endpoint
curl -s "localhost:4318/api/traces/<traceId>"              # full span tree with attributes
```

The OpenAPI spec at `/openapi.json` describes all of it.

The fastest path: copy a trace id (or press "copy digest" on a trace page) and
paste it to the AI. The Claude Code skill in [`skill/SKILL.md`](skill/SKILL.md)
teaches Claude how to read glimpse. It recognizes trace ids and fetches
`/api/traces/{id}/digest` on its own. Copy it to
`~/.claude/skills/glimpse/SKILL.md`. Set `GLIMPSE_URL` if glimpse does not run
on `http://localhost:4318`.

## Licence

MIT, see [LICENSE](LICENSE).
