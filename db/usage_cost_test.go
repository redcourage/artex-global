package db

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

func TestUsageLedgerDeduplicatesInferenceAndPreservesCostSnapshot(t *testing.T) {
	dsn := os.Getenv("ARTEX_PG_DSN")
	if dsn == "" {
		t.Skip("requires isolated ARTEX_PG_DSN")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	if err = d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	task := fmt.Sprintf("metering-test-%d", time.Now().UnixNano())
	defer d.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, task)
	known := true
	u := LLMUsage{TaskID: task, Model: "gpt-6-luna", InputTokens: 1000, OutputTokens: 100, CacheRead: 500, CacheWrite: 100,
		UsageKnown: &known, UsageSource: "codex_subscription", MeteringID: task + ":1", SessionID: "exp1-worker-i3", Status: "ok"}
	for i := 0; i < 2; i++ {
		if err = d.InsertLLMUsage(&u); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := d.TokenByModel(task)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Calls != 1 || rows[0].PricedCalls != 1 || rows[0].Cost == nil {
		t.Fatalf("rows=%+v", rows)
	}
	if math.Abs(rows[0].Cost.TotalUSD-.0001075) > 1e-12 {
		t.Fatalf("cost=%+v", rows[0].Cost)
	}
	var version, session string
	if err = d.QueryRow(`SELECT cost_snapshot->>'price_version',session_id FROM llm_usage WHERE task_id=$1`, task).Scan(&version, &session); err != nil {
		t.Fatal(err)
	}
	if version == "" || session != u.SessionID {
		t.Fatalf("snapshot/session missing: %q %q", version, session)
	}
}

func TestModelUsageCostIsCalculatedPerInference(t *testing.T) {
	known := true
	rows := []LLMUsage{
		{Model: "gpt-6.1-sol", InputTokens: 200000, OutputTokens: 1000, CacheRead: 100000, UsageKnown: &known, UsageSource: "codex_subscription"},
		{Model: "gpt-6.1-sol", InputTokens: 200000, OutputTokens: 1000, CacheRead: 100000, UsageKnown: &known, UsageSource: "codex_subscription"},
	}
	got := aggregateModelUsage(rows)
	if len(got) != 1 || got[0].Cost == nil {
		t.Fatalf("missing cost: %+v", got)
	}
	// Two short requests cost .22 each. Their combined 400K input must NOT
	// select the long-context rate band.
	if math.Abs(got[0].Cost.TotalUSD-.44) > 1e-9 {
		t.Fatalf("cost=%+v", got[0].Cost)
	}
	if got[0].PricedCalls != 2 || got[0].UnpricedCalls != 0 {
		t.Fatalf("coverage=%+v", got[0])
	}
}

func TestModelUsageUnknownAndMissingAreNotZeroCost(t *testing.T) {
	known, missing := true, false
	rows := []LLMUsage{
		{Model: "unknown", InputTokens: 10, UsageKnown: &known},
		{Model: "gpt-6.1-sol", UsageKnown: &missing},
		{Model: "gpt-6.1-sol", InputTokens: 100, UsageKnown: &known},
	}
	got := aggregateModelUsage(rows)
	for _, row := range got {
		if row.Model == "unknown" && (row.Cost != nil || row.UnpricedCalls != 1) {
			t.Fatalf("invented cost: %+v", row)
		}
		if row.Model == "gpt-6.1-sol" && (row.Cost == nil || row.PricedCalls != 1 || row.UnpricedCalls != 1 || row.UsageMissingCalls != 1) {
			t.Fatalf("partial coverage: %+v", row)
		}
	}
}

func TestModelUsagePreservesReportedZeroAndLegacyPositiveUsage(t *testing.T) {
	known := true
	got := aggregateModelUsage([]LLMUsage{
		{Model: "gpt-6-luna", UsageKnown: &known, UsageSource: "api"},
		{Model: "gpt-6-luna", InputTokens: 100},
	})
	if got[0].PricedCalls != 2 || got[0].UsageMissingCalls != 0 {
		t.Fatalf("usage=%+v", got)
	}
}
