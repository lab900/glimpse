package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gendb "github.com/coruben/glimpse/db"
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

	root := spans[0]
	for _, s := range spans {
		if s.ParentSpanID == "" {
			root = s
			break
		}
	}

	svcNames := make([]string, 0, len(services))
	for svc := range services {
		svcNames = append(svcNames, svc)
	}
	sort.Strings(svcNames)

	fmt.Fprintf(&b, "TRACE %s — %s\n", traceID, root.Name)
	fmt.Fprintf(&b, "service: %s | start: %s | total: %s | spans: %d | errors: %d\n\n",
		strings.Join(svcNames, ", "),
		time.Unix(0, t0).UTC().Format(time.RFC3339),
		fmtDur(end-t0), len(spans), errorCount)

	b.WriteString("SPAN TREE (indent = child, @+ = offset from trace start)\n")
	children := map[string][]gendb.Span{}
	byID := map[string]bool{}
	for _, s := range spans {
		byID[s.SpanID] = true
	}
	var roots []gendb.Span
	for _, s := range spans {
		if s.ParentSpanID != "" && byID[s.ParentSpanID] {
			children[s.ParentSpanID] = append(children[s.ParentSpanID], s)
		} else {
			roots = append(roots, s)
		}
	}
	var walk func(s gendb.Span, depth int)
	walk = func(s gendb.Span, depth int) {
		writeDigestSpan(&b, s, depth, t0)
		kids := children[s.SpanID]
		sort.Slice(kids, func(i, j int) bool { return kids[i].StartNs < kids[j].StartNs })
		for _, c := range kids {
			walk(c, depth+1)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].StartNs < roots[j].StartNs })
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
	repeated := false
	for stmt, q := range queries {
		if q.count >= 3 {
			if !repeated {
				b.WriteString("\nREPEATED QUERIES (possible N+1)\n")
				repeated = true
			}
			fmt.Fprintf(&b, "%dx (%s total): %s\n", q.count, fmtDur(q.totalNs), stmt)
		}
	}

	return b.String()
}

func writeDigestSpan(b *strings.Builder, s gendb.Span, depth int, t0 int64) {
	fmt.Fprintf(b, "%s[%s] %s %s @+%s",
		strings.Repeat("  ", depth), spanKindNames[s.Kind], s.Name,
		fmtDur(s.DurationNs), fmtDur(s.StartNs-t0))
	if s.HttpStatus != 0 {
		fmt.Fprintf(b, " http=%d", s.HttpStatus)
	}
	if s.DbStatement != "" {
		fmt.Fprintf(b, " sql=%q", s.DbStatement)
	}
	if s.StatusCode == 2 {
		fmt.Fprintf(b, " ERROR=%q", s.StatusMessage)
	}
	b.WriteString("\n")
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
