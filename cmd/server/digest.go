package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gendb "github.com/lab900-luis-santos/glimpse/db"
)

// handleTraceDigest renders one trace as compact plain text made for feeding
// to an LLM: span tree with offsets, repeated-query detection, error details.
func handleTraceDigest(queries *gendb.Queries) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		traceID := r.PathValue("traceId")
		spans, err := queries.GetTraceSpans(r.Context(), traceID)
		if err != nil {
			http.Error(w, "load failure", http.StatusInternalServerError)
			return
		}
		if len(spans) == 0 {
			http.Error(w, "trace not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(buildDigest(traceID, spans)))
	}
}

func buildDigest(traceID string, spans []gendb.Span) string {
	var b strings.Builder

	t0 := spans[0].StartNs
	end := t0
	services := map[string]bool{}
	errorCount := 0
	for _, s := range spans {
		if s.StartNs < t0 {
			t0 = s.StartNs
		}
		if e := s.StartNs + s.DurationNs; e > end {
			end = e
		}
		services[s.Service] = true
		if s.StatusCode == 2 {
			errorCount++
		}
	}

	children := map[string][]gendb.Span{}
	byID := map[string]bool{}
	for _, s := range spans {
		byID[s.SpanID] = true
	}
	// A root has no parent, or a parent that is not in this trace
	// (for example when the trace id came from an injected traceparent).
	var roots []gendb.Span
	for _, s := range spans {
		if s.ParentSpanID != "" && byID[s.ParentSpanID] {
			children[s.ParentSpanID] = append(children[s.ParentSpanID], s)
		} else {
			roots = append(roots, s)
		}
	}
	sortSpans(roots)

	if len(roots) == 0 { // parent cycle in corrupt data
		roots = spans[:1]
	}
	title := roots[0].Name
	if len(roots) > 1 {
		title += fmt.Sprintf(" (+%d more root spans)", len(roots)-1)
	}
	multiService := len(services) > 1

	svcNames := make([]string, 0, len(services))
	for svc := range services {
		svcNames = append(svcNames, svc)
	}
	sort.Strings(svcNames)

	fmt.Fprintf(&b, "TRACE %s — %s\n", traceID, title)
	fmt.Fprintf(&b, "service: %s | start: %s | total: %s | spans: %d | errors: %d\n\n",
		strings.Join(svcNames, ", "),
		time.Unix(0, t0).UTC().Format(time.RFC3339),
		fmtDur(end-t0), len(spans), errorCount)

	b.WriteString("SPAN TREE (indent = child, @+ = offset from trace start)\n")
	var walk func(s gendb.Span, depth int)
	walk = func(s gendb.Span, depth int) {
		writeDigestSpan(&b, s, depth, t0, multiService)
		kids := children[s.SpanID]
		sortSpans(kids)
		for _, c := range kids {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}

	type qstat struct {
		count   int
		totalNs int64
	}
	queries := map[string]*qstat{}
	for _, s := range spans {
		if s.DbStatement == "" {
			continue
		}
		q := queries[s.DbStatement]
		if q == nil {
			q = &qstat{}
			queries[s.DbStatement] = q
		}
		q.count++
		q.totalNs += s.DurationNs
	}
	var stmts []string
	for stmt, q := range queries {
		if q.count >= 3 {
			stmts = append(stmts, stmt)
		}
	}
	sort.Slice(stmts, func(i, j int) bool {
		if ci, cj := queries[stmts[i]].count, queries[stmts[j]].count; ci != cj {
			return ci > cj
		}
		return stmts[i] < stmts[j]
	})
	if len(stmts) > 0 {
		b.WriteString("\nREPEATED QUERIES (possible N+1)\n")
	}
	for _, stmt := range stmts {
		q := queries[stmt]
		fmt.Fprintf(&b, "%dx (%s total): %s\n", q.count, fmtDur(q.totalNs), stmt)
	}

	return b.String()
}

// sortSpans orders by start time; the span id keeps equal starts deterministic.
func sortSpans(spans []gendb.Span) {
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].StartNs != spans[j].StartNs {
			return spans[i].StartNs < spans[j].StartNs
		}
		return spans[i].SpanID < spans[j].SpanID
	})
}

func writeDigestSpan(b *strings.Builder, s gendb.Span, depth int, t0 int64, withService bool) {
	attrs := spanAttrs(s.Attrs)
	fmt.Fprintf(b, "%s[%s] ", strings.Repeat("  ", depth), spanKindNames[s.Kind])
	if withService {
		fmt.Fprintf(b, "%s: ", s.Service)
	}
	fmt.Fprintf(b, "%s %s @+%s", s.Name, fmtDur(s.DurationNs), fmtDur(s.StartNs-t0))
	if s.Kind == 4 || s.Kind == 5 {
		if dest := firstString(attrs, "messaging.destination.name", "messaging.destination"); dest != "" {
			fmt.Fprintf(b, " dest=%s", dest)
		}
	}
	if s.HttpStatus != 0 {
		fmt.Fprintf(b, " http=%d", s.HttpStatus)
	}
	if s.DbStatement != "" {
		fmt.Fprintf(b, " sql=%q", s.DbStatement)
	}
	if s.StatusCode == 2 {
		fmt.Fprintf(b, " ERROR=%q", errorText(s.StatusMessage, attrs))
	}
	b.WriteString("\n")
}

// spanAttrs decodes the attrs JSON column; a bad value gives an empty map.
func spanAttrs(raw string) map[string]string {
	attrs := map[string]string{}
	json.Unmarshal([]byte(raw), &attrs)
	return attrs
}

func firstString(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := m[k]; v != "" {
			return v
		}
	}
	return ""
}

// errorText is the status message, or the exception recorded at ingest
// ("type: message") when the status message is empty.
func errorText(statusMessage string, attrs map[string]string) string {
	if statusMessage != "" {
		return statusMessage
	}
	typ, msg := attrs["exception.type"], attrs["exception.message"]
	switch {
	case typ != "" && msg != "":
		return typ + ": " + msg
	default:
		return typ + msg
	}
}

func fmtDur(ns int64) string {
	msVal := float64(ns) / 1e6
	if msVal >= 1000 {
		return fmt.Sprintf("%.2fs", msVal/1000)
	}
	if msVal >= 10 {
		return fmt.Sprintf("%.0fms", msVal)
	}
	return fmt.Sprintf("%.1fms", msVal)
}
