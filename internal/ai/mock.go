package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// MockLLMHandler is an optional custom callback for MockLLMProvider.
type MockLLMHandler func(ctx context.Context, req StructuredRequest) (StructuredResponse, error)

// MockLLMProvider provides deterministic zero-cost structured responses for
// tests, evaluation runs, and --dry-run CLI executions.
type MockLLMProvider struct {
	Model   string
	Handler MockLLMHandler
	budget  *BudgetTracker
	calls   atomic.Int64
}

// NewMockLLMProvider creates a MockLLMProvider.
func NewMockLLMProvider(model string, budget *BudgetTracker, handler MockLLMHandler) *MockLLMProvider {
	if strings.TrimSpace(model) == "" {
		model = "mock-editor-v1"
	}
	return &MockLLMProvider{
		Model:   model,
		Handler: handler,
		budget:  budget,
	}
}

// CallCount returns total GenerateStructured invocations.
func (m *MockLLMProvider) CallCount() int64 {
	return m.calls.Load()
}

// GenerateStructured validates input limits/budget and returns a schema-valid
// deterministic response (or delegates to Handler).
func (m *MockLLMProvider) GenerateStructured(ctx context.Context, req StructuredRequest) (StructuredResponse, error) {
	if err := ctx.Err(); err != nil {
		return StructuredResponse{}, err
	}
	estInputTokens := (len(req.SystemPrompt) + len(req.UserPrompt)) / 4
	if req.MaxInputTokens > 0 && estInputTokens > req.MaxInputTokens {
		return StructuredResponse{}, fmt.Errorf("%w: estimated %d > max %d", ErrLLMTokenLimitExceeded, estInputTokens, req.MaxInputTokens)
	}
	if err := m.budget.ReserveOrCheck(0.0001); err != nil {
		return StructuredResponse{}, err
	}

	callNum := m.calls.Add(1)
	if m.Handler != nil {
		resp, err := m.Handler(ctx, req)
		if err != nil {
			return StructuredResponse{}, err
		}
		if err := ValidateAgainstSchema(resp.RawJSON, req.JSONSchema); err != nil {
			return StructuredResponse{}, err
		}
		m.budget.Record(resp.CostUSD)
		return resp, nil
	}

	raw := defaultMockJSON(req)
	if err := ValidateAgainstSchema(raw, req.JSONSchema); err != nil {
		return StructuredResponse{}, err
	}
	return StructuredResponse{
		RequestID:     fmt.Sprintf("mock-req-%d", callNum),
		Model:         m.Model,
		PromptVersion: req.PromptVersion,
		RawJSON:       raw,
		InputTokens:   estInputTokens,
		OutputTokens:  len(raw) / 4,
		CostUSD:       0,
		DurationMS:    1,
	}, nil
}

func defaultMockJSON(req StructuredRequest) json.RawMessage {
	switch req.Operation {
	case "clustering":
		return json.RawMessage(`{"same_event":true,"relationship":"SAME_EVENT","confidence":0.92,"reason":"Mock semantic match on shared entities and title.","canonical_title":"Verified IT News Event","category":"AI"}`)
	case "fact-extraction":
		storyID := extractFieldFromUserPrompt(req.UserPrompt, "story_id:")
		if storyID == "" {
			storyID = "story-mock"
		}
		artID := extractFieldFromUserPrompt(req.UserPrompt, "article_id:")
		if artID == "" {
			artID = "art-mock"
		}
		out, _ := json.Marshal(map[string]any{
			"story_id": storyID,
			"claims": []map[string]any{
				{
					"text":               "Reported primary technology announcement",
					"type":               "EVENT",
					"source_article_ids": []string{artID},
					"entities":           []string{},
					"numbers":            []string{},
					"confidence":         0.9,
				},
				{
					"text":               "Published with verifiable source link",
					"type":               "AVAILABILITY",
					"source_article_ids": []string{artID},
					"entities":           []string{},
					"numbers":            []string{},
					"confidence":         0.85,
				},
			},
		})
		return out
	case "fact-verification":
		storyID := extractFieldFromUserPrompt(req.UserPrompt, "story_id:")
		if storyID == "" {
			storyID = "story-mock"
		}
		out, _ := json.Marshal(map[string]any{
			"story_id": storyID,
			"verdicts": []map[string]any{},
		})
		return out
	case "scoring":
		storyID := extractFieldFromUserPrompt(req.UserPrompt, "story_id:")
		if storyID == "" {
			storyID = "story-mock"
		}
		out, _ := json.Marshal(map[string]any{
			"story_id":          storyID,
			"impact":            78,
			"interest":          75,
			"freshness":         85,
			"novelty":           72,
			"reasoning_summary": "Значимое подтверждённое событие из IT-индустрии.",
			"category":          "AI",
		})
		return out
	default:
		return json.RawMessage(`{}`)
	}
}

func extractFieldFromUserPrompt(prompt, prefix string) string {
	for _, line := range strings.Split(prompt, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
		}
	}
	return ""
}
