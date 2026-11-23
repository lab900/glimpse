package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Execution-plan lookup for Postgres (first version). glimpse asks the server
// to plan the captured statement with EXPLAIN (GENERIC_PLAN, FORMAT JSON), which
// does not execute it. It never uses ANALYZE and always rolls its transaction back.

const (
	engineMSSQL    = "mssql"
	enginePostgres = "postgres"

	// pgMinVersion is PostgreSQL 16, the first version with GENERIC_PLAN.
	pgMinVersion = 160000
	// pgSeqScanRows is the table size from which a filtered Seq Scan is reported.
	pgSeqScanRows = 10000
)

// openPostgres validates the DSN. Connections are opened per request with
// pgx directly: EXPLAIN (GENERIC_PLAN) takes $1 placeholders only as a plain
// simple-protocol query, and database/sql would try to bind arguments.
func openPostgres(dsn string) (*pgx.ConnConfig, error) {
	return pgx.ParseConfig(dsn)
}

// chooseEngine picks the plan engine. With both configured, the span's db
// system decides and MSSQL is the default. The second result is a reason when
// the requested engine is not configured.
func chooseEngine(dbSystem string, mssql *sql.DB, pg *pgx.ConnConfig) (string, string) {
	wantPG := false
	switch strings.ToLower(dbSystem) {
	case "postgresql", "postgres":
		wantPG = true
	}
	switch {
	case wantPG && pg != nil:
		return enginePostgres, ""
	case wantPG:
		return "", "db_system is postgresql but GLIMPSE_POSTGRES_DSN is not set"
	case mssql != nil:
		return engineMSSQL, ""
	case dbSystem == "":
		return enginePostgres, ""
	default:
		return "", "db_system " + dbSystem + " has no plan support configured (GLIMPSE_MSSQL_DSN is not set)"
	}
}

// convertPlaceholders turns the '?' placeholders of a sanitized JDBC statement
// into Postgres $1, $2, ... It leaves quoted strings, quoted identifiers and
// comments alone. It rejects a second statement. Limits: it does not know
// dollar-quoted strings or E” escapes, and it cannot tell a '?' placeholder
// from a Postgres JSON '?' operator.
func convertPlaceholders(stmt string) (string, int, error) {
	var b strings.Builder
	n := 0
	for i := 0; i < len(stmt); i++ {
		c := stmt[i]
		switch {
		case c == '\'' || c == '"':
			end := i + 1
			for {
				j := strings.IndexByte(stmt[end:], c)
				if j < 0 {
					return "", 0, fmt.Errorf("unterminated quote in statement")
				}
				end += j + 1
				if end < len(stmt) && stmt[end] == c { // doubled quote is an escape
					end++
					continue
				}
				break
			}
			b.WriteString(stmt[i:end])
			i = end - 1
		case c == '-' && i+1 < len(stmt) && stmt[i+1] == '-':
			end := strings.IndexByte(stmt[i:], '\n')
			if end < 0 {
				end = len(stmt) - i
			}
			b.WriteString(stmt[i : i+end])
			i += end - 1
		case c == '/' && i+1 < len(stmt) && stmt[i+1] == '*':
			end := strings.Index(stmt[i+2:], "*/")
			if end < 0 {
				return "", 0, fmt.Errorf("unterminated comment in statement")
			}
			b.WriteString(stmt[i : i+end+4])
			i += end + 3
		case c == '?':
			n++
			b.WriteString("$" + strconv.Itoa(n))
		case c == ';':
			if strings.Trim(stmt[i:], "; \t\r\n") != "" {
				return "", 0, fmt.Errorf("only one statement is allowed")
			}
			return b.String(), n, nil
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), n, nil
}

// pgExplainable reports whether stmt starts with a keyword EXPLAIN accepts.
func pgExplainable(stmt string) bool {
	fields := strings.Fields(strings.TrimLeft(stmt, " \t\r\n("))
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(strings.TrimLeft(fields[0], "(")) {
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE":
		return true
	}
	return false
}

// pgSimple runs one simple-protocol query and returns the first cell of the
// first row ("" when there is none).
func pgSimple(ctx context.Context, conn *pgx.Conn, query string) (string, error) {
	results, err := conn.PgConn().Exec(ctx, query).ReadAll()
	if err != nil {
		return "", err
	}
	for _, r := range results {
		if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
			return string(r.Rows[0][0]), nil
		}
	}
	return "", nil
}

// explainPostgres fills out for one captured statement. The error is only for
// connection-level failures; a statement Postgres cannot plan gives found=false.
func explainPostgres(ctx context.Context, pg *pgx.ConnConfig, statement string, out *PlanOutput) (*PlanOutput, error) {
	out.Body.Engine = enginePostgres
	stmt, _, err := convertPlaceholders(statement)
	if err != nil {
		out.Body.Reason = err.Error()
		return out, nil
	}
	if !pgExplainable(stmt) {
		out.Body.Reason = "only SELECT, WITH, INSERT, UPDATE and DELETE statements can be explained"
		return out, nil
	}

	conn, err := pgx.ConnectConfig(ctx, pg)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	defer conn.Close(context.Background())

	// Read-only transaction, always rolled back.
	if _, err := pgSimple(ctx, conn, "BEGIN READ ONLY; SET LOCAL statement_timeout = '3s'"); err != nil {
		return nil, fmt.Errorf("starting postgres transaction: %w", err)
	}
	defer func() { _, _ = pgSimple(context.Background(), conn, "ROLLBACK") }()

	versionNum, err := pgSimple(ctx, conn, "SHOW server_version_num")
	if err != nil {
		return nil, fmt.Errorf("reading postgres version: %w", err)
	}
	if v, _ := strconv.Atoi(versionNum); v < pgMinVersion {
		out.Body.Reason = "PostgreSQL 16 or newer is needed for EXPLAIN (GENERIC_PLAN); server_version_num is " + versionNum
		return out, nil
	}

	planJSON, err := pgSimple(ctx, conn, "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "+stmt)
	if err != nil {
		out.Body.Reason = "postgres cannot plan this statement: " + err.Error()
		return out, nil
	}

	tableRows := func(schema, rel string) float64 {
		name := pgQuoteIdent(rel)
		if schema != "" {
			name = pgQuoteIdent(schema) + "." + name
		}
		var rows float64
		if err := conn.QueryRow(ctx, "SELECT reltuples::float8 FROM pg_class WHERE oid = to_regclass($1)", name).Scan(&rows); err != nil {
			return -1
		}
		return rows
	}
	ops, warnings, err := analyzePGPlan(planJSON, tableRows)
	if err != nil {
		out.Body.Reason = "cannot read the plan returned by postgres: " + err.Error()
		return out, nil
	}
	out.Body.Found = true
	out.Body.PlanJSON = planJSON
	out.Body.Operators = ops
	out.Body.Warnings = warnings

	pgStatementStats(ctx, conn, stmt, out)
	return out, nil
}

func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// pgStatementStats adds execution stats from pg_stat_statements when the
// extension exists and has this statement. Matching is by text with collapsed
// whitespace. Any failure leaves the stats at zero.
func pgStatementStats(ctx context.Context, conn *pgx.Conn, stmt string, out *PlanOutput) {
	var calls int64
	var meanMs, blocks *float64
	err := conn.QueryRow(ctx, `
		SELECT calls, mean_exec_time, (shared_blks_hit + shared_blks_read)::float8 / NULLIF(calls, 0)
		FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND regexp_replace(query, '\s+', ' ', 'g') = $1
		ORDER BY calls DESC LIMIT 1`,
		strings.Join(strings.Fields(stmt), " ")).Scan(&calls, &meanMs, &blocks)
	if err != nil {
		return
	}
	out.Body.ExecutionCount = calls
	if meanMs != nil {
		out.Body.AvgElapsedMs = *meanMs
	}
	if blocks != nil {
		out.Body.AvgLogicalRead = int64(*blocks)
	}
}

// pgPlanNode holds the fields of an EXPLAIN JSON plan node that glimpse reads.
type pgPlanNode struct {
	NodeType     string       `json:"Node Type"`
	RelationName string       `json:"Relation Name"`
	Schema       string       `json:"Schema"`
	Filter       string       `json:"Filter"`
	PlanRows     float64      `json:"Plan Rows"`
	Plans        []pgPlanNode `json:"Plans"`
}

// analyzePGPlan counts node types and reports a filtered Seq Scan on a large
// table. tableRows returns the estimated row count of a relation, or a value
// below zero when unknown (the node's own row estimate is used then).
func analyzePGPlan(planJSON string, tableRows func(schema, rel string) float64) ([]PlanOperator, []string, error) {
	var doc []struct {
		Plan pgPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(planJSON), &doc); err != nil || len(doc) == 0 {
		return nil, nil, fmt.Errorf("unexpected EXPLAIN JSON")
	}

	counts := map[string]int{}
	warnings := []string{}
	var walk func(n pgPlanNode)
	walk = func(n pgPlanNode) {
		counts[n.NodeType]++
		if n.NodeType == "Seq Scan" && n.Filter != "" && n.RelationName != "" {
			rows := tableRows(n.Schema, n.RelationName)
			if rows < 0 {
				rows = n.PlanRows
			}
			if rows >= pgSeqScanRows {
				warnings = append(warnings, fmt.Sprintf(
					"Seq Scan on %s filtered by %s, estimated %.0f rows: an index on those columns may help",
					n.RelationName, n.Filter, rows))
			}
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(doc[0].Plan)

	ops := []PlanOperator{}
	for op, n := range counts {
		ops = append(ops, PlanOperator{Op: op, Count: n})
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Count != ops[j].Count {
			return ops[i].Count > ops[j].Count
		}
		return ops[i].Op < ops[j].Op
	})
	return ops, warnings, nil
}
