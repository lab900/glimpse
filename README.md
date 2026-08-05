# glimpse

A tiny Digma-style trace collector in one Go binary. It receives OTLP/HTTP
traces from an OpenTelemetry agent, stores them in SQLite, and shows code
insights: slow endpoints, N+1 queries, slow SQL, and errors.

## Run

### Option A: download a release (no Go needed)

Grab the binary for your platform from the
[releases page](https://github.com/lab900-luis-santos/glimpse/releases), or
with the GitHub CLI (works for this private repo when you are logged in):

```sh
gh release download --repo lab900-luis-santos/glimpse --pattern '*darwin-arm64*'
tar -xzf glimpse-darwin-arm64.tar.gz
./glimpse-darwin-arm64
```

Current release assets: `glimpse-darwin-arm64` (macOS Apple Silicon). Ask for
other platforms or build one yourself:
`GOOS=linux GOARCH=amd64 go build -o glimpse ./cmd/server`.

### Option B: build from source

```sh
make build      # or: go build -o glimpse ./cmd/server
./glimpse
```

The binary migrates its own database on startup. Everything runs on one port:

- UI:          http://localhost:4318/
- Trace page:  http://localhost:4318/trace/{traceId} (shareable, collapsible span tree)
- OTLP ingest: POST http://localhost:4318/v1/traces (protobuf or JSON, gzip OK)
- JSON API:    /api/endpoints, /api/insights, /api/traces, /api/traces/{traceId}
- AI digest:   /api/traces/{traceId}/digest (plain text, made for LLMs)
- API docs:    http://localhost:4318/docs

Environment variables: `PORT` (default 4318), `DB_PATH` (default
`./data/glimpse.db`), `RETENTION_HOURS` (default 24, old spans are purged).

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

Download the [OpenTelemetry Java agent](https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases)
and start your app with:

```sh
java -javaagent:opentelemetry-javaagent.jar \
  -Dotel.exporter.otlp.protocol=http/protobuf \
  -Dotel.exporter.otlp.endpoint=http://localhost:4318 \
  -Dotel.metrics.exporter=none \
  -Dotel.logs.exporter=none \
  -Dotel.service.name=my-app \
  -jar my-app.jar
```

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
paste it to the AI. A Claude Code skill at `~/.claude/skills/glimpse/SKILL.md`
teaches Claude how to read glimpse — it recognizes trace ids and fetches
`/api/traces/{id}/digest` on its own.
