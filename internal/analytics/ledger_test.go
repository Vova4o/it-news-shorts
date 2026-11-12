package analytics_test

import (
	"errors"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/analytics"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

var ledgerBaseTime = time.Date(2026, time.November, 12, 8, 0, 0, 0, time.UTC)

func buildCost(id string, job domain.VideoJobID, total domain.MicroUSD) domain.GenerationCost {
	return domain.GenerationCost{
		ID:         domain.CostID(id),
		VideoJobID: job,
		Provider:   "llm",
		Operation:  "script",
		Units:      1,
		UnitCost:   total,
		Total:      total,
		CreatedAt:  ledgerBaseTime,
	}
}

func TestLedgerRecordsCosts(t *testing.T) {
	ledger, err := analytics.NewLedger(analytics.Budget{
		MaxDaily:    domain.MicroUSD(10_000),
		MaxPerVideo: domain.MicroUSD(5_000),
	})
	if err != nil {
		t.Fatalf("NewLedger() error = %v", err)
	}
	if err := ledger.Record(buildCost("c1", "vj-01", 1_000)); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := ledger.Record(buildCost("c2", "vj-02", 2_000)); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if got, want := ledger.Total(), domain.MicroUSD(3_000); got != want {
		t.Fatalf("Total() = %s, want %s", got, want)
	}
	if got, want := ledger.ForVideo("vj-01"), domain.MicroUSD(1_000); got != want {
		t.Fatalf("ForVideo() = %s, want %s", got, want)
	}
}

func TestLedgerStopsAtDailyBudget(t *testing.T) {
	ledger, err := analytics.NewLedger(analytics.Budget{
		MaxDaily:    domain.MicroUSD(1_500),
		MaxPerVideo: domain.MicroUSD(1_000),
	})
	if err != nil {
		t.Fatalf("NewLedger() error = %v", err)
	}
	if err := ledger.Record(buildCost("c1", "vj-01", 1_000)); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := ledger.Record(buildCost("c2", "vj-02", 600)); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want %v", err, domain.ErrBudgetExceeded)
	}
	if got, want := ledger.Total(), domain.MicroUSD(1_000); got != want {
		t.Fatalf("Total() = %s, want %s after a rejected cost", got, want)
	}
}

func TestLedgerStopsAtPerVideoBudget(t *testing.T) {
	ledger, err := analytics.NewLedger(analytics.Budget{
		MaxDaily:    domain.MicroUSD(10_000),
		MaxPerVideo: domain.MicroUSD(1_000),
	})
	if err != nil {
		t.Fatalf("NewLedger() error = %v", err)
	}
	if err := ledger.Record(buildCost("c1", "vj-01", 700)); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := ledger.Record(buildCost("c2", "vj-01", 400)); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want %v", err, domain.ErrBudgetExceeded)
	}
}

func TestNewLedgerValidatesBudget(t *testing.T) {
	tests := []struct {
		name   string
		budget analytics.Budget
	}{
		{"zero daily", analytics.Budget{MaxDaily: 0, MaxPerVideo: 1}},
		{"zero per video", analytics.Budget{MaxDaily: 10, MaxPerVideo: 0}},
		{"per video above daily", analytics.Budget{MaxDaily: 100, MaxPerVideo: 200}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := analytics.NewLedger(tt.budget); !errors.Is(err, domain.ErrInvalidValue) {
				t.Fatalf("error = %v, want %v", err, domain.ErrInvalidValue)
			}
		})
	}
}
