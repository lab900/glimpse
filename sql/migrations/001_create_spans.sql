-- +goose Up
CREATE TABLE spans (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id       TEXT NOT NULL,
    span_id        TEXT NOT NULL,
    parent_span_id TEXT NOT NULL DEFAULT '',
    service        TEXT NOT NULL,
    name           TEXT NOT NULL,
    kind           INTEGER NOT NULL DEFAULT 0,
    start_ns       INTEGER NOT NULL,
    duration_ns    INTEGER NOT NULL,
    status_code    INTEGER NOT NULL DEFAULT 0,
    status_message TEXT NOT NULL DEFAULT '',
    http_method    TEXT NOT NULL DEFAULT '',
    http_route     TEXT NOT NULL DEFAULT '',
    http_status    INTEGER NOT NULL DEFAULT 0,
    db_system      TEXT NOT NULL DEFAULT '',
    db_statement   TEXT NOT NULL DEFAULT '',
    attrs          TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_spans_trace ON spans(trace_id);
CREATE INDEX idx_spans_start ON spans(start_ns);
CREATE INDEX idx_spans_server ON spans(kind, start_ns);

-- +goose Down
DROP TABLE spans;
