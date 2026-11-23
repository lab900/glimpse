-- +goose Up
-- OTLP exporters retry, so the same span can arrive twice. Keep the first copy.
DELETE FROM spans WHERE id NOT IN (
    SELECT MIN(id) FROM spans GROUP BY trace_id, span_id
);

-- The unique index starts with trace_id, so it also serves GetTraceSpans.
CREATE UNIQUE INDEX idx_spans_trace_span ON spans(trace_id, span_id);
DROP INDEX idx_spans_trace;

-- +goose Down
CREATE INDEX idx_spans_trace ON spans(trace_id);
DROP INDEX idx_spans_trace_span;
