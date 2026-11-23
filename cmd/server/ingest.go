package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	gendb "github.com/lab900/glimpse/db"
)

const maxBodyBytes = 32 << 20 // 32 MiB per export request

// handleOTLPTraces accepts OTLP/HTTP trace exports (protobuf or JSON, optionally gzip)
// and stores all spans in one transaction.
func handleOTLPTraces(database *sql.DB, queries *gendb.Queries) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := io.Reader(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(body)
			if err != nil {
				http.Error(w, "bad gzip body", http.StatusBadRequest)
				return
			}
			defer gz.Close()
			body = gz
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}

		req := &coltracepb.ExportTraceServiceRequest{}
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		isJSON := mediaType == "application/json"
		if isJSON {
			err = unmarshalOTLPJSON(raw, req)
		} else {
			err = proto.Unmarshal(raw, req)
		}
		if err != nil {
			http.Error(w, "cannot decode OTLP payload: "+err.Error(), http.StatusBadRequest)
			return
		}

		if err := storeSpans(r.Context(), database, queries, req); err != nil {
			log.Printf("store spans: %v", err)
			http.Error(w, "storage failure", http.StatusInternalServerError)
			return
		}

		resp := &coltracepb.ExportTraceServiceResponse{}
		if isJSON {
			out, _ := protojson.Marshal(resp)
			w.Header().Set("Content-Type", "application/json")
			w.Write(out)
		} else {
			out, _ := proto.Marshal(resp)
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.Write(out)
		}
	}
}

// unmarshalOTLPJSON decodes an OTLP/JSON export. The OTLP spec encodes traceId,
// spanId and parentSpanId as hex strings, but protojson expects base64 for
// bytes fields. So the ids are converted from hex to base64 first.
func unmarshalOTLPJSON(raw []byte, req *coltracepb.ExportTraceServiceRequest) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep 64-bit numbers exact
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return err
	}
	for _, rs := range jsonList(doc, "resourceSpans", "resource_spans") {
		for _, ss := range jsonList(rs, "scopeSpans", "scope_spans") {
			for _, span := range jsonList(ss, "spans") {
				for _, key := range []string{"traceId", "trace_id", "spanId", "span_id", "parentSpanId", "parent_span_id"} {
					if err := hexToBase64(span, key); err != nil {
						return err
					}
				}
				for _, link := range jsonList(span, "links") {
					for _, key := range []string{"traceId", "trace_id", "spanId", "span_id"} {
						if err := hexToBase64(link, key); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	converted, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(converted, req)
}

// jsonList returns the objects in the first of the given keys that holds an array.
func jsonList(obj map[string]any, keys ...string) []map[string]any {
	for _, key := range keys {
		list, ok := obj[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// hexToBase64 rewrites obj[key] from a hex string to base64 in place.
func hexToBase64(obj map[string]any, key string) error {
	v, ok := obj[key].(string)
	if !ok {
		return nil
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		return fmt.Errorf("%s %q is not a hex string", key, v)
	}
	obj[key] = base64.StdEncoding.EncodeToString(b)
	return nil
}

func storeSpans(ctx context.Context, database *sql.DB, queries *gendb.Queries, req *coltracepb.ExportTraceServiceRequest) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := queries.WithTx(tx)

	for _, rs := range req.GetResourceSpans() {
		service := attrString(rs.GetResource().GetAttributes(), "service.name")
		if service == "" {
			service = "unknown"
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if err := q.InsertSpan(ctx, toParams(service, span)); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func toParams(service string, span *tracepb.Span) gendb.InsertSpanParams {
	attrs := span.GetAttributes()
	start := int64(span.GetStartTimeUnixNano())
	duration := int64(span.GetEndTimeUnixNano()) - start
	if duration < 0 {
		duration = 0
	}

	method := firstAttr(attrs, "http.request.method", "http.method")
	route := attrString(attrs, "http.route")
	statement := firstAttr(attrs, "db.query.text", "db.statement")
	dbSystem := firstAttr(attrs, "db.system.name", "db.system")

	var httpStatus int64
	for _, key := range []string{"http.response.status_code", "http.status_code"} {
		if v := attrValue(attrs, key); v != nil {
			httpStatus = v.GetIntValue()
			break
		}
	}

	return gendb.InsertSpanParams{
		TraceID:       hex.EncodeToString(span.GetTraceId()),
		SpanID:        hex.EncodeToString(span.GetSpanId()),
		ParentSpanID:  hex.EncodeToString(span.GetParentSpanId()),
		Service:       service,
		Name:          span.GetName(),
		Kind:          int64(span.GetKind()),
		StartNs:       start,
		DurationNs:    duration,
		StatusCode:    int64(span.GetStatus().GetCode()),
		StatusMessage: span.GetStatus().GetMessage(),
		HttpMethod:    method,
		HttpRoute:     route,
		HttpStatus:    httpStatus,
		DbSystem:      dbSystem,
		DbStatement:   statement,
		Attrs:         attrsJSON(attrs, exceptionAttrs(span)),
	}
}

func attrValue(attrs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue()
		}
	}
	return nil
}

func attrString(attrs []*commonpb.KeyValue, key string) string {
	if v := attrValue(attrs, key); v != nil {
		return anyValueString(v)
	}
	return ""
}

func firstAttr(attrs []*commonpb.KeyValue, keys ...string) string {
	for _, key := range keys {
		if s := attrString(attrs, key); s != "" {
			return s
		}
	}
	return ""
}

func anyValueString(v *commonpb.AnyValue) string {
	switch val := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return val.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(val.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(val.BoolValue)
	default:
		return ""
	}
}

// exceptionAttrs returns exception.type and exception.message of the first
// exception event. The OTel Java agent records exceptions as span events.
// The stacktrace is dropped on purpose: it is large.
func exceptionAttrs(span *tracepb.Span) map[string]string {
	for _, ev := range span.GetEvents() {
		if ev.GetName() != "exception" {
			continue
		}
		out := map[string]string{}
		for _, key := range []string{"exception.type", "exception.message"} {
			if v := attrString(ev.GetAttributes(), key); v != "" {
				out[key] = v
			}
		}
		return out
	}
	return nil
}

// attrsJSON flattens span attributes to a JSON object. Extra entries
// (exception info) only fill keys the span does not already have.
func attrsJSON(attrs []*commonpb.KeyValue, extra map[string]string) string {
	if len(attrs) == 0 && len(extra) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(attrs)+len(extra))
	for _, kv := range attrs {
		m[kv.GetKey()] = anyValueString(kv.GetValue())
	}
	for k, v := range extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(out)
}
