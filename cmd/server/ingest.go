package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	gendb "github.com/coruben/glimpse/db"
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
		contentType := r.Header.Get("Content-Type")
		if contentType == "application/json" {
			err = protojson.Unmarshal(raw, req)
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
		if contentType == "application/json" {
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
		Attrs:         attrsJSON(attrs),
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

func attrsJSON(attrs []*commonpb.KeyValue) string {
	if len(attrs) == 0 {
		return "{}"
	}
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[kv.GetKey()] = anyValueString(kv.GetValue())
	}
	out, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(out)
}
