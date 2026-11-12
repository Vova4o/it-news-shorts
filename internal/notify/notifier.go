package notify

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// MessageKind classifies notifications.
type MessageKind string

// Notification kinds.
const (
	KindDailyReport MessageKind = "DAILY_REPORT"
	KindFailure     MessageKind = "FAILURE"
	KindBudgetAlert MessageKind = "BUDGET_ALERT"
)

// Valid reports whether the kind is known.
func (k MessageKind) Valid() bool {
	switch k {
	case KindDailyReport, KindFailure, KindBudgetAlert:
		return true
	default:
		return false
	}
}

// Message is one Telegram notification.
type Message struct {
	Kind     MessageKind
	Subject  string
	Body     string
	Metadata map[string]string
}

// Validate enforces the message rules.
func (m Message) Validate() error {
	if !m.Kind.Valid() {
		return domain.InvalidValue("message.kind")
	}
	if strings.TrimSpace(m.Subject) == "" {
		return domain.MissingField("message.subject")
	}
	if strings.TrimSpace(m.Body) == "" {
		return domain.MissingField("message.body")
	}
	return nil
}

// Notifier delivers notifications. The Telegram implementation arrives later.
type Notifier interface {
	Notify(ctx context.Context, msg Message) error
}

// DailyReport summarizes one run for the operator.
type DailyReport struct {
	RunID     domain.DailyRunID
	RunDate   time.Time
	Stories   int
	Published []domain.Publication
	Failed    []domain.VideoJobID
	TotalCost domain.MicroUSD
}

// Message renders the report as a notification.
func (r DailyReport) Message() (Message, error) {
	if r.RunID == "" {
		return Message{}, domain.MissingField("daily_report.run_id")
	}
	if r.RunDate.IsZero() {
		return Message{}, domain.MissingField("daily_report.run_date")
	}
	if err := r.TotalCost.Validate(); err != nil {
		return Message{}, err
	}
	for _, publication := range r.Published {
		if err := publication.Validate(); err != nil {
			return Message{}, err
		}
	}
	subject := "Отчёт за " + r.RunDate.Format("2006-01-02")
	body := "Опубликовано: " + itoa(len(r.Published)) + ", ошибок: " + itoa(len(r.Failed)) +
		", расход: " + r.TotalCost.String() + " USD"
	return Message{
		Kind:    KindDailyReport,
		Subject: subject,
		Body:    body,
		Metadata: map[string]string{
			"run_id": string(r.RunID),
		},
	}, nil
}

func itoa(v int) string {
	return strconv.Itoa(v)
}
