package analytics

import (
	"fmt"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// Budget is the hard cost limit of one run.
type Budget struct {
	MaxDaily    domain.MicroUSD
	MaxPerVideo domain.MicroUSD
}

// Validate enforces the budget rules.
func (b Budget) Validate() error {
	if b.MaxDaily <= 0 {
		return domain.InvalidValue("budget.max_daily")
	}
	if b.MaxPerVideo <= 0 {
		return domain.InvalidValue("budget.max_per_video")
	}
	if b.MaxPerVideo > b.MaxDaily {
		return domain.InvalidValue("budget.max_per_video")
	}
	return nil
}

// Ledger accumulates generation costs and stops the pipeline before a budget
// is exceeded. Costs are recorded before the provider call, so a runaway loop
// cannot spend money.
type Ledger struct {
	budget Budget
	total  domain.MicroUSD
	perJob map[domain.VideoJobID]domain.MicroUSD
}

// NewLedger validates the budget and returns an empty ledger.
func NewLedger(budget Budget) (*Ledger, error) {
	if err := budget.Validate(); err != nil {
		return nil, err
	}
	return &Ledger{
		budget: budget,
		perJob: make(map[domain.VideoJobID]domain.MicroUSD),
	}, nil
}

// Record adds a cost entry or fails with domain.ErrBudgetExceeded.
func (l *Ledger) Record(cost domain.GenerationCost) error {
	if err := cost.Validate(); err != nil {
		return err
	}
	nextTotal := l.total + cost.Total
	nextJob := l.perJob[cost.VideoJobID] + cost.Total
	if nextTotal > l.budget.MaxDaily {
		return fmt.Errorf("%w: daily limit %s, requested total %s",
			domain.ErrBudgetExceeded, l.budget.MaxDaily, nextTotal)
	}
	if nextJob > l.budget.MaxPerVideo {
		return fmt.Errorf("%w: per video limit %s, requested total %s",
			domain.ErrBudgetExceeded, l.budget.MaxPerVideo, nextJob)
	}
	l.total = nextTotal
	l.perJob[cost.VideoJobID] = nextJob
	return nil
}

// Total returns the accumulated cost.
func (l *Ledger) Total() domain.MicroUSD { return l.total }

// ForVideo returns the accumulated cost of one job.
func (l *Ledger) ForVideo(id domain.VideoJobID) domain.MicroUSD { return l.perJob[id] }
