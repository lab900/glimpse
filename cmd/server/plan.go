package main

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	_ "github.com/microsoft/go-mssqldb"
)

// Execution-plan lookup. For MSSQL glimpse reads the plan cache DMVs
// (read-only, needs VIEW SERVER STATE). For Postgres see plan_pg.go.
// Captured SQL is never executed.

type PlanInput struct {
	Body struct {
		Statement string `json:"statement" required:"true" minLength:"10" doc:"Captured db.statement to look up"`
		DBSystem  string `json:"db_system,omitempty" doc:"db.system of the span (postgresql, mssql, microsoft.sql_server). Picks the engine when both are configured; MSSQL is the default."`
	}
}

type MissingIndex struct {
	ImpactPct float64 `json:"impact_pct" doc:"Predicted cost reduction in percent"`
	Table     string  `json:"table"`
	DDL       string  `json:"ddl" doc:"Ready-to-run CREATE INDEX statement"`
}

type PlanOperator struct {
	Op    string `json:"op"`
	Count int    `json:"count"`
}

type PlanOutput struct {
	Body struct {
		Engine         string         `json:"engine,omitempty" enum:"mssql,postgres" doc:"Which database produced the plan"`
		Found          bool           `json:"found"`
		Reason         string         `json:"reason,omitempty" doc:"Why no plan was found"`
		ExecutionCount int64          `json:"execution_count,omitempty"`
		AvgElapsedMs   float64        `json:"avg_elapsed_ms,omitempty"`
		AvgLogicalRead int64          `json:"avg_logical_reads,omitempty" doc:"8KB pages read per execution"`
		LastExecution  string         `json:"last_execution,omitempty"`
		MissingIndexes []MissingIndex `json:"missing_indexes"`
		Warnings       []string       `json:"warnings"`
		Operators      []PlanOperator `json:"operators" doc:"Notable operators in the plan"`
		PlanXML        string         `json:"plan_xml,omitempty" doc:"MSSQL showplan XML"`
		PlanJSON       string         `json:"plan_json,omitempty" doc:"Postgres EXPLAIN (FORMAT JSON) output"`
	}
}

func registerPlanAPI(api huma.API, mssql *sql.DB, pg *pgx.ConnConfig) {
	huma.Post(api, "/api/queryplan", func(ctx context.Context, in *PlanInput) (*PlanOutput, error) {
		ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		out := &PlanOutput{}
		out.Body.MissingIndexes = []MissingIndex{}
		out.Body.Warnings = []string{}
		out.Body.Operators = []PlanOperator{}

		engine, reason := chooseEngine(in.Body.DBSystem, mssql, pg)
		if engine == "" {
			out.Body.Reason = reason
			return out, nil
		}
		if engine == enginePostgres {
			res, err := explainPostgres(ctx, pg, in.Body.Statement, out)
			if err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
			return res, nil
		}
		out.Body.Engine = engineMSSQL

		row := mssql.QueryRowContext(ctx, `
			SELECT TOP 1 qp.query_plan, qs.execution_count,
			       qs.total_elapsed_time, qs.total_logical_reads, qs.last_execution_time
			FROM sys.dm_exec_query_stats qs
			CROSS APPLY sys.dm_exec_sql_text(qs.sql_handle) st
			CROSS APPLY sys.dm_exec_query_plan(qs.plan_handle) qp
			WHERE qp.query_plan IS NOT NULL AND st.text LIKE @pattern
			ORDER BY qs.total_worker_time DESC`,
			sql.Named("pattern", likePattern(in.Body.Statement)))

		var planXML string
		var execCount, totalElapsedUs, totalReads int64
		var lastExec time.Time
		err := row.Scan(&planXML, &execCount, &totalElapsedUs, &totalReads, &lastExec)
		if err == sql.ErrNoRows {
			out.Body.Reason = "no cached plan matches this statement — run the query again, then retry"
			return out, nil
		}
		if err != nil {
			return nil, huma.Error502BadGateway("plan cache query failed: " + err.Error())
		}

		out.Body.Found = true
		out.Body.ExecutionCount = execCount
		if execCount > 0 {
			out.Body.AvgElapsedMs = float64(totalElapsedUs) / float64(execCount) / 1000
			out.Body.AvgLogicalRead = totalReads / execCount
		}
		out.Body.LastExecution = lastExec.UTC().Format(time.RFC3339)
		out.Body.PlanXML = planXML

		parsed := parseShowplan(planXML)
		out.Body.MissingIndexes = parsed.missing
		out.Body.Warnings = parsed.warnings
		out.Body.Operators = parsed.operators
		return out, nil
	})
}

// likePattern builds a LIKE pattern from a captured statement. The JDBC driver
// rewrites '?' placeholders to @P0/@P1 before SQL Server sees them, so each '?'
// becomes a wildcard. The cached text also carries a "(@P0 type, …)" parameter
// declaration prolog, hence the leading wildcard. Long statements are cut to a
// distinctive prefix.
func likePattern(stmt string) string {
	var b strings.Builder
	for _, r := range stmt {
		switch r {
		case '%', '_', '[':
			b.WriteString("[" + string(r) + "]")
		case '?':
			b.WriteString("%")
		default:
			b.WriteRune(r)
		}
	}
	p := b.String()
	if len(p) > 500 {
		p = p[:500]
		// do not cut inside a [x] escape
		if i := strings.LastIndex(p, "["); i > strings.LastIndex(p, "]") {
			p = p[:i]
		}
	}
	return "%" + p + "%"
}

type parsedPlan struct {
	missing   []MissingIndex
	warnings  []string
	operators []PlanOperator
}

var notableOps = map[string]bool{
	"Table Scan": true, "Clustered Index Scan": true, "Index Scan": true,
	"Key Lookup": true, "RID Lookup": true, "Sort": true, "Hash Match": true,
	"Index Seek": true, "Clustered Index Seek": true, "Nested Loops": true,
}

// parseShowplan token-walks the showplan XML: extracts missing-index
// suggestions, warning elements, and counts of notable operators.
func parseShowplan(planXML string) parsedPlan {
	res := parsedPlan{missing: []MissingIndex{}, warnings: []string{}, operators: []PlanOperator{}}
	dec := xml.NewDecoder(strings.NewReader(planXML))
	// Plans often declare encoding="utf-16" while the driver already delivered
	// UTF-8 — accept any declared charset as-is.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) { return input, nil }

	opCounts := map[string]int{}
	warnSet := map[string]bool{}
	var curImpact float64
	var curTable, curSchema string
	var eqCols, ineqCols, inclCols []string
	var curUsage string
	inWarnings := false

	attr := func(el xml.StartElement, name string) string {
		for _, a := range el.Attr {
			if a.Name.Local == name {
				return a.Value
			}
		}
		return ""
	}
	flushIndex := func() {
		if curTable == "" {
			return
		}
		res.missing = append(res.missing, MissingIndex{
			ImpactPct: curImpact,
			Table:     curSchema + "." + curTable,
			DDL:       buildIndexDDL(curSchema, curTable, eqCols, ineqCols, inclCols),
		})
		curTable, curSchema = "", ""
		eqCols, ineqCols, inclCols = nil, nil, nil
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "RelOp":
				if op := attr(el, "PhysicalOp"); op != "" && notableOps[op] {
					opCounts[op]++
				}
			case "MissingIndexGroup":
				fmt.Sscanf(attr(el, "Impact"), "%f", &curImpact)
			case "MissingIndex":
				curSchema = strings.Trim(attr(el, "Schema"), "[]")
				curTable = strings.Trim(attr(el, "Table"), "[]")
			case "ColumnGroup":
				curUsage = attr(el, "Usage")
			case "Column":
				if curTable != "" {
					col := strings.Trim(attr(el, "Name"), "[]")
					switch curUsage {
					case "EQUALITY":
						eqCols = append(eqCols, col)
					case "INEQUALITY":
						ineqCols = append(ineqCols, col)
					case "INCLUDE":
						inclCols = append(inclCols, col)
					}
				}
			case "Warnings":
				inWarnings = true
				for _, a := range el.Attr {
					if a.Value == "true" || a.Value == "1" {
						warnSet[a.Name.Local] = true
					}
				}
			default:
				// detail elements inside a warning, not warnings themselves
				if inWarnings && el.Name.Local != "ColumnReference" {
					warnSet[el.Name.Local] = true
				}
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "MissingIndex":
				flushIndex()
			case "Warnings":
				inWarnings = false
			}
		}
	}

	for op, n := range opCounts {
		res.operators = append(res.operators, PlanOperator{Op: op, Count: n})
	}
	for w := range warnSet {
		res.warnings = append(res.warnings, w)
	}
	return res
}

func buildIndexDDL(schema, table string, eq, ineq, incl []string) string {
	key := append(append([]string{}, eq...), ineq...)
	name := "ix_glimpse_" + strings.ToLower(table) + "_" + strings.ToLower(strings.Join(key, "_"))
	if len(name) > 120 {
		name = name[:120]
	}
	ddl := fmt.Sprintf("CREATE INDEX %s ON %s.%s (%s)", name, schema, table, strings.Join(key, ", "))
	if len(incl) > 0 {
		ddl += " INCLUDE (" + strings.Join(incl, ", ") + ")"
	}
	return ddl
}
