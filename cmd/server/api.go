package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"

	gendb "github.com/lab900/glimpse/db"
)

type WindowInput struct {
	WindowMinutes int `query:"window_minutes" default:"60" minimum:"1" maximum:"10080" doc:"Look-back window in minutes"`
}

func (in *WindowInput) sinceNs() int64 {
	return time.Now().Add(-time.Duration(in.WindowMinutes) * time.Minute).UnixNano()
}

type Endpoint struct {
	Service    string  `json:"service"`
	Method     string  `json:"method"`
	Route      string  `json:"route"`
	Name       string  `json:"name" doc:"Span name, e.g. 'GET /owners/{id}'"`
	Calls      int64   `json:"calls"`
	ErrorCount int64   `json:"error_count"`
	AvgMs      float64 `json:"avg_ms"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	MaxMs      float64 `json:"max_ms"`
	LastSeen   string  `json:"last_seen"`
}

type EndpointsOutput struct {
	Body struct {
		Endpoints []Endpoint `json:"endpoints"`
	}
}

type TraceSummary struct {
	TraceID    string  `json:"trace_id"`
	Service    string  `json:"service"`
	Name       string  `json:"name"`
	HTTPStatus int64   `json:"http_status"`
	Error      bool    `json:"error"`
	Start      string  `json:"start"`
	DurationMs float64 `json:"duration_ms"`
}

type ListTracesInput struct {
	Endpoint string `query:"endpoint" doc:"Filter by span name, e.g. 'GET /owners/{id}'. Empty means all."`
	Limit    int    `query:"limit" default:"20" minimum:"1" maximum:"200"`
	WindowInput
}

type ListTracesOutput struct {
	Body struct {
		Traces []TraceSummary `json:"traces"`
	}
}

type GetTraceInput struct {
	TraceID string `path:"traceId" doc:"Hex trace id"`
}

type TraceSpan struct {
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Service      string            `json:"service"`
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Start        string            `json:"start"`
	StartNs      int64             `json:"start_ns"`
	DurationMs   float64           `json:"duration_ms"`
	Error        bool              `json:"error"`
	StatusMsg    string            `json:"status_message,omitempty"`
	Attrs        map[string]string `json:"attrs"`
}

type GetTraceOutput struct {
	Body struct {
		TraceID string      `json:"trace_id"`
		Spans   []TraceSpan `json:"spans"`
	}
}

type NPlusOne struct {
	TraceID   string  `json:"trace_id"`
	DbSystem  string  `json:"db_system"`
	Statement string  `json:"statement"`
	Reps      int64   `json:"repetitions"`
	TotalMs   float64 `json:"total_ms"`
	LastSeen  string  `json:"last_seen"`
}

type SlowQuery struct {
	DbSystem     string  `json:"db_system"`
	Statement    string  `json:"statement"`
	Calls        int64   `json:"calls"`
	AvgMs        float64 `json:"avg_ms"`
	MaxMs        float64 `json:"max_ms"`
	ExampleTrace string  `json:"example_trace" doc:"Trace id of the slowest occurrence"`
}

type ErrorSpan struct {
	TraceID    string  `json:"trace_id"`
	Service    string  `json:"service"`
	Name       string  `json:"name"`
	Message    string  `json:"message"`
	Route      string  `json:"route,omitempty"`
	Start      string  `json:"start"`
	DurationMs float64 `json:"duration_ms"`
}

type InsightsOutput struct {
	Body struct {
		NPlusOne    []NPlusOne  `json:"n_plus_one" doc:"Traces that repeat the same SQL statement 5+ times"`
		SlowQueries []SlowQuery `json:"slow_queries"`
		Errors      []ErrorSpan `json:"errors"`
	}
}

type ServiceSpans struct {
	Service string `json:"service"`
	Spans   int64  `json:"spans"`
}

type StatsOutput struct {
	Body struct {
		Version        string         `json:"version"`
		TotalSpans     int64          `json:"total_spans"`
		DBBytes        int64          `json:"db_bytes" doc:"Size of the SQLite database (page_count * page_size)"`
		SpansByService []ServiceSpans `json:"spans_by_service"`
		MssqlPlans     bool           `json:"mssql_plans" doc:"True when GLIMPSE_MSSQL_DSN is configured"`
		PostgresPlans  bool           `json:"postgres_plans" doc:"True when GLIMPSE_POSTGRES_DSN is configured"`
	}
}

var plansEnabled, postgresPlansEnabled bool

var spanKindNames = map[int64]string{
	0: "unspecified", 1: "internal", 2: "server", 3: "client", 4: "producer", 5: "consumer",
}

func ms(ns int64) float64 { return float64(ns) / 1e6 }

func tsRFC3339(ns int64) string {
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

func registerAPI(api huma.API, database *sql.DB, queries *gendb.Queries) {
	huma.Get(api, "/api/endpoints", func(ctx context.Context, in *WindowInput) (*EndpointsOutput, error) {
		since := in.sinceNs()
		rows, err := queries.ListEndpoints(ctx, since)
		if err != nil {
			return nil, fmt.Errorf("listing endpoints: %w", err)
		}
		durations, err := queries.ListServerDurations(ctx, since)
		if err != nil {
			return nil, fmt.Errorf("listing durations: %w", err)
		}
		percentiles := computePercentiles(durations)

		out := &EndpointsOutput{}
		out.Body.Endpoints = []Endpoint{}
		for _, r := range rows {
			p := percentiles[r.Service+"\x00"+r.Name]
			out.Body.Endpoints = append(out.Body.Endpoints, Endpoint{
				Service:    r.Service,
				Method:     r.HttpMethod,
				Route:      r.HttpRoute,
				Name:       r.Name,
				Calls:      r.Calls,
				ErrorCount: r.Errors,
				AvgMs:      ms(r.AvgNs),
				P50Ms:      ms(p.p50),
				P95Ms:      ms(p.p95),
				MaxMs:      ms(r.MaxNs),
				LastSeen:   tsRFC3339(r.LastSeenNs),
			})
		}
		return out, nil
	})

	huma.Get(api, "/api/traces", func(ctx context.Context, in *ListTracesInput) (*ListTracesOutput, error) {
		rows, err := queries.ListRecentTraces(ctx, gendb.ListRecentTracesParams{
			Endpoint: in.Endpoint,
			SinceNs:  in.sinceNs(),
			MaxRows:  int64(in.Limit),
		})
		if err != nil {
			return nil, fmt.Errorf("listing traces: %w", err)
		}
		out := &ListTracesOutput{}
		out.Body.Traces = []TraceSummary{}
		for _, r := range rows {
			out.Body.Traces = append(out.Body.Traces, TraceSummary{
				TraceID:    r.TraceID,
				Service:    r.Service,
				Name:       r.Name,
				HTTPStatus: r.HttpStatus,
				Error:      r.StatusCode == 2,
				Start:      tsRFC3339(r.StartNs),
				DurationMs: ms(r.DurationNs),
			})
		}
		return out, nil
	})

	huma.Get(api, "/api/traces/{traceId}", func(ctx context.Context, in *GetTraceInput) (*GetTraceOutput, error) {
		rows, err := queries.GetTraceSpans(ctx, in.TraceID)
		if err != nil {
			return nil, fmt.Errorf("loading trace: %w", err)
		}
		if len(rows) == 0 {
			return nil, huma.Error404NotFound("trace not found")
		}
		out := &GetTraceOutput{}
		out.Body.TraceID = in.TraceID
		for _, r := range rows {
			attrs := map[string]string{}
			json.Unmarshal([]byte(r.Attrs), &attrs)
			out.Body.Spans = append(out.Body.Spans, TraceSpan{
				SpanID:       r.SpanID,
				ParentSpanID: r.ParentSpanID,
				Service:      r.Service,
				Name:         r.Name,
				Kind:         spanKindNames[r.Kind],
				Start:        tsRFC3339(r.StartNs),
				StartNs:      r.StartNs,
				DurationMs:   ms(r.DurationNs),
				Error:        r.StatusCode == 2,
				StatusMsg:    r.StatusMessage,
				Attrs:        attrs,
			})
		}
		return out, nil
	})

	huma.Get(api, "/api/insights", func(ctx context.Context, in *WindowInput) (*InsightsOutput, error) {
		since := in.sinceNs()
		out := &InsightsOutput{}
		out.Body.NPlusOne = []NPlusOne{}
		out.Body.SlowQueries = []SlowQuery{}
		out.Body.Errors = []ErrorSpan{}

		nplus, err := queries.ListNPlusOne(ctx, since)
		if err != nil {
			return nil, fmt.Errorf("listing n+1: %w", err)
		}
		for _, r := range nplus {
			out.Body.NPlusOne = append(out.Body.NPlusOne, NPlusOne{
				TraceID:   r.TraceID,
				DbSystem:  r.DbSystem,
				Statement: r.DbStatement,
				Reps:      r.Reps,
				TotalMs:   ms(r.TotalNs),
				LastSeen:  tsRFC3339(r.LastSeenNs),
			})
		}

		slow, err := queries.ListSlowQueries(ctx, since)
		if err != nil {
			return nil, fmt.Errorf("listing slow queries: %w", err)
		}
		for _, r := range slow {
			out.Body.SlowQueries = append(out.Body.SlowQueries, SlowQuery{
				DbSystem:     r.DbSystem,
				Statement:    r.DbStatement,
				Calls:        r.Calls,
				AvgMs:        ms(r.AvgNs),
				MaxMs:        ms(r.MaxNs),
				ExampleTrace: r.ExampleTrace,
			})
		}

		errs, err := queries.ListErrorSpans(ctx, since)
		if err != nil {
			return nil, fmt.Errorf("listing errors: %w", err)
		}
		for _, r := range errs {
			out.Body.Errors = append(out.Body.Errors, ErrorSpan{
				TraceID:    r.TraceID,
				Service:    r.Service,
				Name:       r.Name,
				Message:    errorText(r.StatusMessage, spanAttrs(r.Attrs)),
				Route:      r.HttpRoute,
				Start:      tsRFC3339(r.StartNs),
				DurationMs: ms(r.DurationNs),
			})
		}
		return out, nil
	})

	huma.Delete(api, "/api/spans", func(ctx context.Context, in *struct{}) (*struct{}, error) {
		if err := queries.DeleteAllSpans(ctx); err != nil {
			return nil, fmt.Errorf("wiping spans: %w", err)
		}
		return nil, nil
	})

	huma.Get(api, "/api/stats", func(ctx context.Context, in *struct{}) (*StatsOutput, error) {
		total, err := queries.CountSpans(ctx)
		if err != nil {
			return nil, fmt.Errorf("counting spans: %w", err)
		}
		var dbBytes int64
		if err := database.QueryRowContext(ctx,
			"SELECT (SELECT page_count FROM pragma_page_count) * (SELECT page_size FROM pragma_page_size)",
		).Scan(&dbBytes); err != nil {
			return nil, fmt.Errorf("reading database size: %w", err)
		}
		byService, err := queries.CountSpansByService(ctx)
		if err != nil {
			return nil, fmt.Errorf("counting spans by service: %w", err)
		}
		out := &StatsOutput{}
		out.Body.Version = version
		out.Body.TotalSpans = total
		out.Body.DBBytes = dbBytes
		out.Body.SpansByService = []ServiceSpans{}
		for _, r := range byService {
			out.Body.SpansByService = append(out.Body.SpansByService, ServiceSpans{Service: r.Service, Spans: r.Spans})
		}
		out.Body.MssqlPlans = plansEnabled
		out.Body.PostgresPlans = postgresPlansEnabled
		return out, nil
	})
}

type percentilePair struct {
	p50, p95 int64
}

// computePercentiles expects rows sorted by (service, name, duration_ns),
// which ListServerDurations guarantees.
func computePercentiles(rows []gendb.ListServerDurationsRow) map[string]percentilePair {
	result := map[string]percentilePair{}
	var key string
	var group []int64

	flush := func() {
		if len(group) == 0 {
			return
		}
		result[key] = percentilePair{
			p50: group[(len(group)-1)*50/100],
			p95: group[(len(group)-1)*95/100],
		}
	}

	for _, r := range rows {
		k := r.Service + "\x00" + r.Name
		if k != key {
			flush()
			key = k
			group = group[:0]
		}
		group = append(group, r.DurationNs)
	}
	flush()
	return result
}
