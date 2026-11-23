package main

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestConvertPlaceholders(t *testing.T) {
	tests := []struct {
		in, want string
		n        int
		wantErr  bool
	}{
		{in: "select * from t where a = ? and b = ?", want: "select * from t where a = $1 and b = $2", n: 2},
		{in: "select '?' , \"c?\" from t where a = ?", want: "select '?' , \"c?\" from t where a = $1", n: 1},
		{in: "select 'it''s ?' from t where a=?;", want: "select 'it''s ?' from t where a=$1", n: 1},
		{in: "select 1 -- why?\nfrom t where a = ?", want: "select 1 -- why?\nfrom t where a = $1", n: 1},
		{in: "select /* ? */ ?", want: "select /* ? */ $1", n: 1},
		{in: "select 1; delete from t", wantErr: true},
		{in: "select 'open", wantErr: true},
		{in: "select /* open", wantErr: true},
	}
	for _, tc := range tests {
		got, n, err := convertPlaceholders(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want || n != tc.n {
			t.Errorf("%q: got %q, %d, %v; want %q, %d", tc.in, got, n, err, tc.want, tc.n)
		}
	}
}

func TestPgExplainable(t *testing.T) {
	for _, s := range []string{"select 1", "  WITH x AS (select 1) select * from x", "(select 1)", "update t set a=1", "Delete from t", "insert into t values (1)"} {
		if !pgExplainable(s) {
			t.Errorf("%q should be explainable", s)
		}
	}
	for _, s := range []string{"drop table t", "create index i on t(a)", "explain select 1", "call p()", ""} {
		if pgExplainable(s) {
			t.Errorf("%q should not be explainable", s)
		}
	}
}

const samplePGPlan = `[{"Plan": {"Node Type": "Nested Loop", "Plan Rows": 10, "Plans": [
  {"Node Type": "Seq Scan", "Relation Name": "t", "Schema": "public", "Filter": "(name = $1)", "Plan Rows": 5},
  {"Node Type": "Seq Scan", "Relation Name": "small", "Schema": "public", "Filter": "(id = 1)", "Plan Rows": 1},
  {"Node Type": "Seq Scan", "Relation Name": "nofilter", "Schema": "public", "Plan Rows": 900000},
  {"Node Type": "Index Scan", "Relation Name": "u", "Schema": "public", "Plan Rows": 1}
]}}]`

func TestAnalyzePGPlan(t *testing.T) {
	rows := func(schema, rel string) float64 {
		switch rel {
		case "t":
			return 1000000
		case "small":
			return 50
		}
		return -1
	}
	ops, warnings, err := analyzePGPlan(samplePGPlan, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 || ops[0].Op != "Seq Scan" || ops[0].Count != 3 {
		t.Errorf("operators: %v", ops)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Seq Scan on t") ||
		!strings.Contains(warnings[0], "(name = $1)") || !strings.Contains(warnings[0], "1000000") {
		t.Errorf("warnings: %v", warnings)
	}

	// unknown table size falls back to the node estimate
	_, warnings, _ = analyzePGPlan(samplePGPlan, func(string, string) float64 { return -1 })
	if len(warnings) != 0 {
		t.Errorf("warnings with unknown sizes: %v", warnings)
	}

	if _, _, err := analyzePGPlan("not json", rows); err == nil {
		t.Error("bad JSON should fail")
	}
}

func TestChooseEngine(t *testing.T) {
	ms, pg := &sql.DB{}, &pgx.ConnConfig{}
	tests := []struct {
		sys      string
		mssql    *sql.DB
		pg       *pgx.ConnConfig
		want     string
		wantNote bool
	}{
		{"", ms, pg, engineMSSQL, false},
		{"postgresql", ms, pg, enginePostgres, false},
		{"microsoft.sql_server", ms, pg, engineMSSQL, false},
		{"", nil, pg, enginePostgres, false},
		{"", ms, nil, engineMSSQL, false},
		{"postgresql", ms, nil, "", true},
		{"mssql", nil, pg, "", true},
	}
	for _, tc := range tests {
		got, reason := chooseEngine(tc.sys, tc.mssql, tc.pg)
		if got != tc.want || (reason != "") != tc.wantNote {
			t.Errorf("%q: got %q, %q", tc.sys, got, reason)
		}
	}
}
