package main

import (
	"strings"
	"testing"
)

const sampleShowplan = `<?xml version="1.0" encoding="utf-16"?>
<ShowPlanXML xmlns="http://schemas.microsoft.com/sqlserver/2004/07/showplan" Version="1.564">
  <BatchSequence><Batch><Statements>
    <StmtSimple StatementText="select ..." StatementType="SELECT">
      <QueryPlan>
        <MissingIndexes>
          <MissingIndexGroup Impact="94.32">
            <MissingIndex Database="[stip-backend-invoice]" Schema="[invoicing]" Table="[translation]">
              <ColumnGroup Usage="EQUALITY">
                <Column Name="[uuid]" ColumnId="1"/>
              </ColumnGroup>
              <ColumnGroup Usage="INCLUDE">
                <Column Name="[en]" ColumnId="2"/>
                <Column Name="[fr]" ColumnId="3"/>
              </ColumnGroup>
            </MissingIndex>
          </MissingIndexGroup>
        </MissingIndexes>
        <Warnings NoJoinPredicate="true"/>
        <RelOp NodeId="0" PhysicalOp="Hash Match" LogicalOp="Inner Join" EstimateRows="120">
          <RelOp NodeId="1" PhysicalOp="Clustered Index Scan" LogicalOp="Clustered Index Scan" EstimateRows="5000"/>
          <RelOp NodeId="2" PhysicalOp="Index Seek" LogicalOp="Index Seek" EstimateRows="1"/>
        </RelOp>
      </QueryPlan>
    </StmtSimple>
  </Statements></Batch></BatchSequence>
</ShowPlanXML>`

func TestParseShowplan(t *testing.T) {
	p := parseShowplan(sampleShowplan)

	if len(p.missing) != 1 {
		t.Fatalf("missing indexes: got %d, want 1", len(p.missing))
	}
	mi := p.missing[0]
	if mi.ImpactPct != 94.32 {
		t.Errorf("impact: got %v, want 94.32", mi.ImpactPct)
	}
	if mi.Table != "invoicing.translation" {
		t.Errorf("table: got %q", mi.Table)
	}
	wantDDL := "CREATE INDEX ix_glimpse_translation_uuid ON invoicing.translation (uuid) INCLUDE (en, fr)"
	if mi.DDL != wantDDL {
		t.Errorf("ddl:\n got %q\nwant %q", mi.DDL, wantDDL)
	}

	ops := map[string]int{}
	for _, o := range p.operators {
		ops[o.Op] = o.Count
	}
	if ops["Hash Match"] != 1 || ops["Clustered Index Scan"] != 1 || ops["Index Seek"] != 1 {
		t.Errorf("operators: got %v", p.operators)
	}

	if len(p.warnings) != 1 || p.warnings[0] != "NoJoinPredicate" {
		t.Errorf("warnings: got %v", p.warnings)
	}
}

func TestLikePattern(t *testing.T) {
	got := likePattern("select t1_0.[uuid] from [invoicing].[translation] t1_0 where t1_0.[uuid]=?")
	if !strings.HasSuffix(got, "=%%") && !strings.HasSuffix(got, "%") {
		t.Errorf("pattern should end with wildcard: %q", got)
	}
	if !strings.Contains(got, "[[]uuid]") {
		t.Errorf("brackets should be escaped: %q", got)
	}
	if strings.Contains(got, "?") {
		t.Errorf("placeholders should become wildcards: %q", got)
	}

	long := likePattern(strings.Repeat("abcdefghij", 100))
	if len(long) > 510 {
		t.Errorf("long pattern not truncated: %d chars", len(long))
	}
}
