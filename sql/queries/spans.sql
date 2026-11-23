-- name: InsertSpan :exec
INSERT INTO spans (
    trace_id, span_id, parent_span_id, service, name, kind,
    start_ns, duration_ns, status_code, status_message,
    http_method, http_route, http_status, db_system, db_statement, attrs
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (trace_id, span_id) DO NOTHING;

-- name: ListEndpoints :many
SELECT service, http_method, http_route, name, kind,
    COUNT(*)                                            AS calls,
    CAST(AVG(duration_ns) AS INTEGER)                   AS avg_ns,
    CAST(MAX(duration_ns) AS INTEGER)                   AS max_ns,
    CAST(SUM(CASE WHEN status_code = 2 THEN 1 ELSE 0 END) AS INTEGER) AS errors,
    CAST(MAX(start_ns) AS INTEGER)                      AS last_seen_ns
FROM spans
WHERE kind = 2 AND start_ns >= ?
GROUP BY service, http_method, http_route, name
ORDER BY calls DESC;

-- name: ListServerDurations :many
SELECT service, name, duration_ns
FROM spans
WHERE kind = 2 AND start_ns >= ?
ORDER BY service, name, duration_ns;

-- name: ListRecentTraces :many
SELECT trace_id, service, name, http_method, http_route, http_status,
    start_ns, duration_ns, status_code
FROM spans
WHERE kind = 2 AND (name = @endpoint OR @endpoint = '') AND start_ns >= @since_ns
ORDER BY start_ns DESC
LIMIT @max_rows;

-- name: GetTraceSpans :many
SELECT * FROM spans
WHERE trace_id = ?
ORDER BY start_ns ASC;

-- name: ListNPlusOne :many
SELECT trace_id, db_statement,
    COUNT(*)          AS reps,
    CAST(SUM(duration_ns) AS INTEGER)  AS total_ns,
    CAST(MAX(start_ns) AS INTEGER)     AS last_seen_ns
FROM spans
WHERE db_statement != '' AND start_ns >= ?
GROUP BY trace_id, db_statement
HAVING COUNT(*) >= 5
ORDER BY reps DESC
LIMIT 50;

-- name: ListSlowQueries :many
-- With exactly one MAX() in the query, SQLite takes bare columns
-- (example_trace) from the row that holds the max. AVG/COUNT do not count.
SELECT db_system, db_statement,
    COUNT(*)                          AS calls,
    CAST(AVG(duration_ns) AS INTEGER) AS avg_ns,
    CAST(MAX(duration_ns) AS INTEGER) AS max_ns,
    trace_id                          AS example_trace
FROM spans
WHERE db_statement != '' AND start_ns >= ?
GROUP BY db_system, db_statement
ORDER BY avg_ns DESC
LIMIT 50;

-- name: ListErrorSpans :many
SELECT trace_id, service, name, status_message, http_route,
    start_ns, duration_ns, attrs
FROM spans
WHERE status_code = 2 AND start_ns >= ?
ORDER BY start_ns DESC
LIMIT 100;

-- name: DeleteAllSpans :exec
DELETE FROM spans;

-- name: PurgeOldSpans :exec
DELETE FROM spans WHERE start_ns < ?;

-- name: CountSpans :one
SELECT COUNT(*) FROM spans;

-- name: CountSpansByService :many
SELECT service, COUNT(*) AS spans
FROM spans
GROUP BY service
ORDER BY spans DESC, service;
