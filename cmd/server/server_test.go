package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	glimpse "github.com/lab900/glimpse"
	gendb "github.com/lab900/glimpse/db"
)

const testTrace = "0123456789abcdef0123456789abcdef"

// newTestServer returns a router on a real temporary SQLite database
// that went through the embedded migrations.
func newTestServer(t *testing.T) (http.Handler, *gendb.Queries) {
	t.Helper()
	database, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := gendb.New(database)
	return newRouter(database, queries, nil, nil), queries
}

func strAttr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	if _, err := fmt.Sscanf(s, "%x", &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// testSpan builds a span. spanID is a 16-char hex string, parent may be "".
func testSpan(t *testing.T, traceID, spanID, parent, name string, kind tracepb.Span_SpanKind, start time.Time, dur time.Duration, attrs ...*commonpb.KeyValue) *tracepb.Span {
	t.Helper()
	s := &tracepb.Span{
		TraceId:           hexBytes(t, traceID),
		SpanId:            hexBytes(t, spanID),
		Name:              name,
		Kind:              kind,
		StartTimeUnixNano: uint64(start.UnixNano()),
		EndTimeUnixNano:   uint64(start.Add(dur).UnixNano()),
		Attributes:        attrs,
	}
	if parent != "" {
		s.ParentSpanId = hexBytes(t, parent)
	}
	return s
}

func exportReq(service string, spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   &resourcepb.Resource{Attributes: []*commonpb.KeyValue{strAttr("service.name", service)}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
}

func post(t *testing.T, h http.Handler, contentType string, gz bool, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		w.Write(body)
		w.Close()
		body = buf.Bytes()
	}
	req := httptest.NewRequest("POST", "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func export(t *testing.T, h http.Handler, req *coltracepb.ExportTraceServiceRequest) {
	t.Helper()
	body, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(t, h, "application/x-protobuf", false, body); rec.Code != 200 {
		t.Fatalf("export: status %d: %s", rec.Code, rec.Body)
	}
}

func get(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != 200 {
		t.Fatalf("GET %s: status %d: %s", path, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func spansOf(t *testing.T, q *gendb.Queries, traceID string) []gendb.Span {
	t.Helper()
	spans, err := q.GetTraceSpans(t.Context(), traceID)
	if err != nil {
		t.Fatal(err)
	}
	return spans
}

// otlpJSON builds an OTLP/JSON export the way the spec says: ids as hex strings.
func otlpJSON(traceID, spanID, name string, start time.Time) []byte {
	return []byte(fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"svc-a"}}]},`+
		`"scopeSpans":[{"spans":[{"traceId":%q,"spanId":%q,"name":%q,"kind":2,`+
		`"startTimeUnixNano":"%d","endTimeUnixNano":"%d"}]}]}]}`,
		traceID, spanID, name, start.UnixNano(), start.Add(time.Millisecond).UnixNano()))
}

func TestOTLPJSONSpecHexIds(t *testing.T) {
	h, q := newTestServer(t)
	const traceID = "5b8efff798038103d269b633813fc60c"
	body := otlpJSON(traceID, "eee19b7ec3c1b174", "GET /spec", time.Now())
	body = bytes.Replace(body, []byte(`"name"`), []byte(`"parentSpanId":"eee19b7ec3c1b173","name"`), 1)
	if rec := post(t, h, "application/json", false, body); rec.Code != 200 {
		t.Fatalf("export: status %d: %s", rec.Code, rec.Body)
	}

	spans := spansOf(t, q, traceID)
	if len(spans) != 1 || spans[0].SpanID != "eee19b7ec3c1b174" || spans[0].ParentSpanID != "eee19b7ec3c1b173" {
		t.Fatalf("stored spans: %+v", spans)
	}
	if got := get(t, h, "/api/traces/"+traceID); !strings.Contains(got, traceID) || !strings.Contains(got, "GET /spec") {
		t.Fatalf("trace api: %s", got)
	}
	if got := get(t, h, "/api/traces/"+traceID+"/digest"); !strings.Contains(got, "GET /spec") {
		t.Fatalf("digest: %s", got)
	}

	bad := bytes.Replace(body, []byte(traceID), []byte("zz8efff798038103d269b633813fc60c"), 1)
	if rec := post(t, h, "application/json", false, bad); rec.Code != 400 {
		t.Fatalf("non-hex id: status %d, want 400", rec.Code)
	}
}

func TestIngestJSONCharsetAndGzip(t *testing.T) {
	h, q := newTestServer(t)
	now := time.Now()
	jsonBody := otlpJSON(testTrace, "00000000000000a1", "GET /a", now)

	rec := post(t, h, "application/json; charset=utf-8", false, jsonBody)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("json+charset: status %d, content-type %q, body %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}

	jsonBody2 := otlpJSON(testTrace, "00000000000000a2", "GET /b", now)
	if rec := post(t, h, "application/json", true, jsonBody2); rec.Code != 200 {
		t.Fatalf("gzip json: status %d: %s", rec.Code, rec.Body)
	}

	spans := spansOf(t, q, testTrace)
	if len(spans) != 2 || spans[0].Service != "svc-a" || spans[0].SpanID != "00000000000000a1" {
		t.Fatalf("stored spans: %+v", spans)
	}

	if rec := post(t, h, "application/json", false, []byte("not json")); rec.Code != 400 {
		t.Fatalf("bad payload: status %d, want 400", rec.Code)
	}
}

func TestDuplicateExportStoredOnce(t *testing.T) {
	h, q := newTestServer(t)
	req := exportReq("svc-a", testSpan(t, testTrace, "00000000000000a1", "", "GET /a", tracepb.Span_SPAN_KIND_SERVER, time.Now(), time.Millisecond))
	export(t, h, req)
	export(t, h, req)
	if n := len(spansOf(t, q, testTrace)); n != 1 {
		t.Fatalf("stored %d spans, want 1", n)
	}
}

func TestExceptionEventCaptured(t *testing.T) {
	h, q := newTestServer(t)
	now := time.Now()
	failing := testSpan(t, testTrace, "00000000000000a1", "", "GET /boom", tracepb.Span_SPAN_KIND_SERVER, now, time.Millisecond)
	failing.Status = &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR}
	failing.Events = []*tracepb.Span_Event{
		{Name: "other", Attributes: []*commonpb.KeyValue{strAttr("exception.type", "wrong")}},
		{Name: "exception", Attributes: []*commonpb.KeyValue{
			strAttr("exception.type", "java.lang.IllegalStateException"),
			strAttr("exception.message", "dossier locked"),
			strAttr("exception.stacktrace", "at a.b.C(C.java:1)"),
		}},
	}
	export(t, h, exportReq("svc-a", failing))

	spans := spansOf(t, q, testTrace)
	attrs := spanAttrs(spans[0].Attrs)
	if attrs["exception.type"] != "java.lang.IllegalStateException" || attrs["exception.message"] != "dossier locked" {
		t.Fatalf("attrs: %v", attrs)
	}
	if _, ok := attrs["exception.stacktrace"]; ok {
		t.Fatal("stacktrace must not be stored")
	}

	digest := get(t, h, "/api/traces/"+testTrace+"/digest")
	if !strings.Contains(digest, `ERROR="java.lang.IllegalStateException: dossier locked"`) {
		t.Fatalf("digest lacks exception:\n%s", digest)
	}

	var insights struct {
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.Unmarshal([]byte(get(t, h, "/api/insights")), &insights); err != nil {
		t.Fatal(err)
	}
	if len(insights.Errors) != 1 || insights.Errors[0].Message != "java.lang.IllegalStateException: dossier locked" {
		t.Fatalf("insights errors: %+v", insights.Errors)
	}
}

func TestDigestMultiServiceMultiRoot(t *testing.T) {
	h, _ := newTestServer(t)
	now := time.Now()
	sql := func(id, stmt string, off int) *tracepb.Span {
		return testSpan(t, testTrace, id, "00000000000000b1", "SELECT", tracepb.Span_SPAN_KIND_CLIENT,
			now.Add(time.Duration(off)*time.Millisecond), time.Millisecond, strAttr("db.statement", stmt))
	}
	// Roots: parents 00..ff and 00..ee are not in the trace (injected traceparent).
	export(t, h, exportReq("dossier",
		testSpan(t, testTrace, "00000000000000b1", "00000000000000ff", "GET /x", tracepb.Span_SPAN_KIND_SERVER, now, 50*time.Millisecond),
		sql("00000000000000c1", "select b", 1), sql("00000000000000c2", "select b", 2), sql("00000000000000c3", "select b", 3),
		sql("00000000000000c4", "select a", 4), sql("00000000000000c5", "select a", 5), sql("00000000000000c6", "select a", 6),
		sql("00000000000000c7", "select z", 7), sql("00000000000000c8", "select z", 8), sql("00000000000000c9", "select z", 9), sql("00000000000000ca", "select z", 10),
		testSpan(t, testTrace, "00000000000000d1", "00000000000000b1", "dossier.events send", tracepb.Span_SPAN_KIND_PRODUCER,
			now.Add(20*time.Millisecond), time.Millisecond, strAttr("messaging.destination.name", "queue.events")),
	))
	export(t, h, exportReq("billing",
		testSpan(t, testTrace, "00000000000000e1", "00000000000000ee", "billing.events process", tracepb.Span_SPAN_KIND_CONSUMER,
			now.Add(30*time.Millisecond), 5*time.Millisecond, strAttr("messaging.destination", "queue.events")),
	))

	path := "/api/traces/" + testTrace + "/digest"
	first := get(t, h, path)
	for i := 0; i < 20; i++ {
		if again := get(t, h, path); again != first {
			t.Fatalf("digest is not deterministic:\n%s\n---\n%s", first, again)
		}
	}

	for _, want := range []string{
		"TRACE " + testTrace + " — GET /x (+1 more root spans)",
		"service: billing, dossier",
		"[server] dossier: GET /x",
		"[producer] dossier: dossier.events send",
		" dest=queue.events",
		"[consumer] billing: billing.events process",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("digest lacks %q:\n%s", want, first)
		}
	}
	// 4x z first, then the 3x pair ordered by statement.
	z, a, b := strings.Index(first, "4x"), strings.Index(first, "select a"), strings.Index(first, "3x (3.0ms total): select b")
	if z < 0 || a < 0 || b < 0 || !(z < strings.Index(first, "3x (3.0ms total): select a") && strings.Index(first, "3x (3.0ms total): select a") < b) {
		t.Errorf("repeated queries order wrong:\n%s", first)
	}
}

func TestDigestSingleServiceFormat(t *testing.T) {
	h, _ := newTestServer(t)
	now := time.Now()
	export(t, h, exportReq("only",
		testSpan(t, testTrace, "00000000000000b1", "", "GET /x", tracepb.Span_SPAN_KIND_SERVER, now, time.Millisecond)))
	digest := get(t, h, "/api/traces/"+testTrace+"/digest")
	if !strings.Contains(digest, "[server] GET /x ") || strings.Contains(digest, "more root") {
		t.Fatalf("unexpected digest:\n%s", digest)
	}
}

func TestComputePercentiles(t *testing.T) {
	var rows []gendb.ListServerDurationsRow
	for i := int64(1); i <= 100; i++ {
		rows = append(rows, gendb.ListServerDurationsRow{Service: "a", Name: "n1", DurationNs: i})
	}
	rows = append(rows,
		gendb.ListServerDurationsRow{Service: "a", Name: "n2", DurationNs: 7},
		gendb.ListServerDurationsRow{Service: "b", Name: "n1", DurationNs: 3},
		gendb.ListServerDurationsRow{Service: "b", Name: "n1", DurationNs: 9},
	)
	got := computePercentiles(rows)
	if p := got["a\x00n1"]; p.p50 != 50 || p.p95 != 95 {
		t.Errorf("a/n1: %+v", p)
	}
	if p := got["a\x00n2"]; p.p50 != 7 || p.p95 != 7 {
		t.Errorf("a/n2: %+v", p)
	}
	if p := got["b\x00n1"]; p.p50 != 3 || p.p95 != 3 {
		t.Errorf("b/n1: %+v", p)
	}
	if len(computePercentiles(nil)) != 0 {
		t.Error("empty input must give empty result")
	}
}

func TestSlowQueryExampleIsSlowest(t *testing.T) {
	h, q := newTestServer(t)
	now := time.Now()
	stmt := strAttr("db.statement", "select slow")
	// The slowest occurrence is neither first nor last, and not the lowest id.
	durs := []time.Duration{5, 3, 90, 7, 2}
	for i, d := range durs {
		trace := fmt.Sprintf("%032x", i+1)
		export(t, h, exportReq("svc", testSpan(t, trace, "00000000000000a1", "", "SELECT",
			tracepb.Span_SPAN_KIND_CLIENT, now, d*time.Millisecond, stmt, strAttr("db.system", "postgresql"))))
	}
	rows, err := q.ListSlowQueries(t.Context(), now.Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ExampleTrace != fmt.Sprintf("%032x", 3) || rows[0].MaxNs != int64(90*time.Millisecond) {
		t.Fatalf("slow queries: %+v", rows)
	}
}

func TestStats(t *testing.T) {
	h, _ := newTestServer(t)
	now := time.Now()
	export(t, h, exportReq("svc-a",
		testSpan(t, testTrace, "00000000000000a1", "", "x", tracepb.Span_SPAN_KIND_SERVER, now, time.Millisecond),
		testSpan(t, testTrace, "00000000000000a2", "", "y", tracepb.Span_SPAN_KIND_SERVER, now, time.Millisecond)))
	export(t, h, exportReq("svc-b",
		testSpan(t, testTrace, "00000000000000b1", "", "z", tracepb.Span_SPAN_KIND_SERVER, now, time.Millisecond)))

	var stats struct {
		Version        string `json:"version"`
		TotalSpans     int64  `json:"total_spans"`
		DBBytes        int64  `json:"db_bytes"`
		SpansByService []struct {
			Service string `json:"service"`
			Spans   int64  `json:"spans"`
		} `json:"spans_by_service"`
	}
	if err := json.Unmarshal([]byte(get(t, h, "/api/stats")), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Version != version || stats.TotalSpans != 3 || stats.DBBytes <= 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if len(stats.SpansByService) != 2 || stats.SpansByService[0].Service != "svc-a" || stats.SpansByService[0].Spans != 2 || stats.SpansByService[1].Spans != 1 {
		t.Fatalf("spans_by_service: %+v", stats.SpansByService)
	}
}

// A database created by the old schema (001 only) with duplicate spans
// must migrate and keep one copy of each span.
func TestMigrateOldDatabaseWithDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(glimpse.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(old, "sql/migrations", 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"s1", "s1", "s2", "s1"} {
		if _, err := old.Exec(`INSERT INTO spans (trace_id, span_id, service, name, start_ns, duration_ns)
			VALUES ('t', ?, 'svc', 'n', 1, 1)`, id); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	database, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var n int
	if err := database.QueryRow("SELECT COUNT(*) FROM spans").Scan(&n); err != nil || n != 2 {
		t.Fatalf("spans after migration: %d, %v", n, err)
	}
	rows, err := database.Query("EXPLAIN QUERY PLAN SELECT * FROM spans WHERE trace_id = 't' ORDER BY start_ns")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		rows.Scan(&id, &parent, &unused, &detail)
		plan.WriteString(detail + "\n")
	}
	if !strings.Contains(plan.String(), "idx_spans_trace_span") {
		t.Fatalf("GetTraceSpans does not use the unique index:\n%s", plan.String())
	}
}
