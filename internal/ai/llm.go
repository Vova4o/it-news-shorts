package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Typed LLM errors for retry classification, budget enforcement, and testing.
var (
	ErrLLMAuth               = errors.New("ai: llm authentication failed")
	ErrLLMRateLimited        = errors.New("ai: llm rate limited")
	ErrLLMTimeout            = errors.New("ai: llm request timed out")
	ErrLLMInvalidJSON        = errors.New("ai: llm returned invalid JSON")
	ErrLLMSchemaMismatch     = errors.New("ai: llm response failed JSON schema validation")
	ErrLLMEmptyResponse      = errors.New("ai: llm returned empty response")
	ErrLLMBudgetExceeded     = errors.New("ai: llm daily budget exceeded")
	ErrLLMTokenLimitExceeded = errors.New("ai: llm input token limit exceeded")
	ErrLLMUpstream           = errors.New("ai: llm upstream service error")
)

// StructuredRequest describes a schema-constrained LLM call.
type StructuredRequest struct {
	Operation       string
	Model           string
	PromptVersion   string
	SystemPrompt    string
	UserPrompt      string
	JSONSchema      json.RawMessage
	MaxInputTokens  int
	MaxOutputTokens int
	Temperature     float64
}

// StructuredResponse holds the validated JSON output and usage telemetry.
type StructuredResponse struct {
	RequestID     string
	Model         string
	PromptVersion string
	RawJSON       json.RawMessage
	InputTokens   int
	OutputTokens  int
	CostUSD       float64
	DurationMS    int64
}

// LLMProvider abstracts structured JSON-schema LLM generation.
type LLMProvider interface {
	GenerateStructured(ctx context.Context, request StructuredRequest) (StructuredResponse, error)
}

// BudgetTracker enforces a daily USD spend ceiling across concurrent LLM calls.
type BudgetTracker struct {
	mu       sync.Mutex
	limitUSD float64
	spentUSD float64
}

// NewBudgetTracker creates a BudgetTracker with a daily USD limit (0 = unlimited).
func NewBudgetTracker(limitUSD float64) *BudgetTracker {
	return &BudgetTracker{limitUSD: limitUSD}
}

// ReserveOrCheck fails with ErrLLMBudgetExceeded if adding estimatedCostUSD
// would exceed the daily budget.
func (b *BudgetTracker) ReserveOrCheck(estimatedCostUSD float64) error {
	if b == nil || b.limitUSD <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spentUSD+estimatedCostUSD > b.limitUSD {
		return fmt.Errorf("%w: spent=%.6f estimated=%.6f limit=%.6f", ErrLLMBudgetExceeded, b.spentUSD, estimatedCostUSD, b.limitUSD)
	}
	return nil
}

// Record adds actual spent USD after a completed LLM request.
func (b *BudgetTracker) Record(actualCostUSD float64) {
	if b == nil || actualCostUSD <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spentUSD += actualCostUSD
}

// Spent returns total recorded spend in USD.
func (b *BudgetTracker) Spent() float64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spentUSD
}

// ValidateAgainstSchema verifies that rawJSON is valid JSON and satisfies the
// top-level object requirements, types, enums, and numeric bounds of schemaJSON.
func ValidateAgainstSchema(rawJSON, schemaJSON json.RawMessage) error {
	if len(rawJSON) == 0 {
		return ErrLLMEmptyResponse
	}
	var payload any
	if err := json.Unmarshal(rawJSON, &payload); err != nil {
		return fmt.Errorf("%w: %v", ErrLLMInvalidJSON, err)
	}
	if len(schemaJSON) == 0 {
		return nil
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		return fmt.Errorf("ai: invalid schema definition: %w", err)
	}
	if err := validateValue(payload, schema, "$"); err != nil {
		return fmt.Errorf("%w: %v", ErrLLMSchemaMismatch, err)
	}
	return nil
}

func validateValue(val any, schema map[string]any, path string) error {
	typ, _ := schema["type"].(string)
	switch typ {
	case "object":
		obj, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object", path)
		}
		if reqRaw, ok := schema["required"].([]any); ok {
			for _, r := range reqRaw {
				field, _ := r.(string)
				if _, exists := obj[field]; !exists {
					return fmt.Errorf("%s: missing required field %q", path, field)
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		addProps, hasAdd := schema["additionalProperties"].(bool)
		if hasAdd && !addProps && props != nil {
			for k := range obj {
				if _, allowed := props[k]; !allowed {
					return fmt.Errorf("%s: unexpected property %q", path, k)
				}
			}
		}
		for k, v := range obj {
			if props == nil {
				continue
			}
			if subRaw, ok := props[k].(map[string]any); ok {
				if err := validateValue(v, subRaw, path+"."+k); err != nil {
					return err
				}
			}
		}
	case "array":
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array", path)
		}
		if minItems, ok := schema["minItems"].(float64); ok && float64(len(arr)) < minItems {
			return fmt.Errorf("%s: array length %d < minItems %d", path, len(arr), int(minItems))
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for i, elem := range arr {
				if err := validateValue(elem, itemSchema, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("%s: expected string", path)
		}
		if minLen, ok := schema["minLength"].(float64); ok && float64(len([]rune(s))) < minLen {
			return fmt.Errorf("%s: string shorter than minLength %d", path, int(minLen))
		}
		if enumRaw, ok := schema["enum"].([]any); ok && len(enumRaw) > 0 {
			matched := false
			for _, e := range enumRaw {
				if es, ok := e.(string); ok && es == s {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("%s: %q not in enum", path, s)
			}
		}
	case "number":
		n, ok := val.(float64)
		if !ok {
			return fmt.Errorf("%s: expected number", path)
		}
		if minVal, ok := schema["minimum"].(float64); ok && n < minVal {
			return fmt.Errorf("%s: %v < minimum %v", path, n, minVal)
		}
		if maxVal, ok := schema["maximum"].(float64); ok && n > maxVal {
			return fmt.Errorf("%s: %v > maximum %v", path, n, maxVal)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("%s: expected boolean", path)
		}
	}
	return nil
}
