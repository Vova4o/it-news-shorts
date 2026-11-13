package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
)

func TestOpenAIProvider_AllScenarios(t *testing.T) {
	reg, err := LoadEmbeddedPrompts()
	if err != nil {
		t.Fatalf("LoadEmbeddedPrompts() error = %v", err)
	}
	scoringPrompt, err := reg.Get("scoring")
	if err != nil {
		t.Fatalf("Get(scoring) error = %v", err)
	}

	baseReq := StructuredRequest{
		Operation:       "scoring",
		Model:           "gpt-4o-mini",
		PromptVersion:   scoringPrompt.Version,
		SystemPrompt:    scoringPrompt.SystemInstructions,
		UserPrompt:      "story_id: s1\ntitle: Test",
		JSONSchema:      scoringPrompt.OutputSchema,
		MaxInputTokens:  4000,
		MaxOutputTokens: 500,
	}

	t.Run("success structured response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer sk-test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{
				"id":"chatcmpl-1",
				"model":"gpt-4o-mini",
				"choices":[{"message":{"content":"{\"story_id\":\"s1\",\"impact\":80,\"interest\":75,\"freshness\":90,\"novelty\":70,\"reasoning_summary\":\"ok\",\"category\":\"AI\"}"}}],
				"usage":{"prompt_tokens":100,"completion_tokens":50}
			}`))
		}))
		defer srv.Close()

		budget := NewBudgetTracker(1.0)
		p := NewOpenAIProvider(config.LLMConfig{
			Model:                 "gpt-4o-mini",
			APIKey:                "sk-test",
			BaseURL:               srv.URL,
			Timeout:               5 * time.Second,
			MaxConcurrentRequests: 2,
			InputCostPer1MUSD:     0.15,
			OutputCostPer1MUSD:    0.60,
		}, budget, nil)

		resp, err := p.GenerateStructured(context.Background(), baseReq)
		if err != nil {
			t.Fatalf("GenerateStructured() error = %v", err)
		}
		if resp.RequestID != "chatcmpl-1" || resp.InputTokens != 100 || resp.OutputTokens != 50 {
			t.Fatalf("unexpected response: %+v", resp)
		}
		if budget.Spent() <= 0 {
			t.Fatalf("expected positive budget spent, got %f", budget.Spent())
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"content":"{not-json"}}]}`))
		}))
		defer srv.Close()
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "sk", BaseURL: srv.URL, Timeout: 2 * time.Second}, nil, nil)
		_, err := p.GenerateStructured(context.Background(), baseReq)
		if !errors.Is(err, ErrLLMInvalidJSON) {
			t.Fatalf("expected ErrLLMInvalidJSON, got %v", err)
		}
	})

	t.Run("schema mismatch", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// impact > 100 violates schema maximum
			_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"content":"{\"story_id\":\"s1\",\"impact\":150,\"interest\":75,\"freshness\":90,\"novelty\":70,\"reasoning_summary\":\"ok\",\"category\":\"AI\"}"}}]}`))
		}))
		defer srv.Close()
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "sk", BaseURL: srv.URL, Timeout: 2 * time.Second}, nil, nil)
		_, err := p.GenerateStructured(context.Background(), baseReq)
		if !errors.Is(err, ErrLLMSchemaMismatch) {
			t.Fatalf("expected ErrLLMSchemaMismatch, got %v", err)
		}
	})

	t.Run("empty response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"id":"1","choices":[{"message":{"content":"   "}}]}`))
		}))
		defer srv.Close()
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "sk", BaseURL: srv.URL, Timeout: 2 * time.Second}, nil, nil)
		_, err := p.GenerateStructured(context.Background(), baseReq)
		if !errors.Is(err, ErrLLMEmptyResponse) {
			t.Fatalf("expected ErrLLMEmptyResponse, got %v", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "sk", BaseURL: srv.URL, Timeout: 40 * time.Millisecond}, nil, nil)
		p.maxRetries = 0
		_, err := p.GenerateStructured(context.Background(), baseReq)
		if !errors.Is(err, ErrLLMTimeout) {
			t.Fatalf("expected ErrLLMTimeout, got %v", err)
		}
	})

	t.Run("retry on 429 and 503", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if n == 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"content":"{\"story_id\":\"s1\",\"impact\":80,\"interest\":75,\"freshness\":90,\"novelty\":70,\"reasoning_summary\":\"ok\",\"category\":\"AI\"}"}}]}`))
		}))
		defer srv.Close()
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "sk", BaseURL: srv.URL, Timeout: 2 * time.Second}, nil, nil)
		p.baseWait = 5 * time.Millisecond
		resp, err := p.GenerateStructured(context.Background(), baseReq)
		if err != nil {
			t.Fatalf("expected recovery after retries, got %v", err)
		}
		if resp.RequestID != "ok" || calls.Load() != 3 {
			t.Fatalf("calls=%d resp=%+v", calls.Load(), resp)
		}
	})

	t.Run("auth error no retry", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "bad", BaseURL: srv.URL, Timeout: 2 * time.Second}, nil, nil)
		_, err := p.GenerateStructured(context.Background(), baseReq)
		if !errors.Is(err, ErrLLMAuth) || calls.Load() != 1 {
			t.Fatalf("expected single attempt ErrLLMAuth, got calls=%d err=%v", calls.Load(), err)
		}
	})

	t.Run("context cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "sk", BaseURL: "https://example.invalid", Timeout: 2 * time.Second}, nil, nil)
		_, err := p.GenerateStructured(ctx, baseReq)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("token limit exceeded", func(t *testing.T) {
		p := NewOpenAIProvider(config.LLMConfig{APIKey: "sk", BaseURL: "https://example.invalid", Timeout: 2 * time.Second}, nil, nil)
		smallReq := baseReq
		smallReq.MaxInputTokens = 2
		_, err := p.GenerateStructured(context.Background(), smallReq)
		if !errors.Is(err, ErrLLMTokenLimitExceeded) {
			t.Fatalf("expected ErrLLMTokenLimitExceeded, got %v", err)
		}
	})

	t.Run("budget exceeded", func(t *testing.T) {
		budget := NewBudgetTracker(0.0001)
		budget.Record(0.0001)
		p := NewOpenAIProvider(config.LLMConfig{
			APIKey:             "sk",
			BaseURL:            "https://example.invalid",
			Timeout:            2 * time.Second,
			InputCostPer1MUSD:  1.0,
			OutputCostPer1MUSD: 1.0,
		}, budget, nil)
		_, err := p.GenerateStructured(context.Background(), baseReq)
		if !errors.Is(err, ErrLLMBudgetExceeded) {
			t.Fatalf("expected ErrLLMBudgetExceeded, got %v", err)
		}
	})

	t.Run("mock provider schema validation", func(t *testing.T) {
		m := NewMockLLMProvider("", NewBudgetTracker(1.0), nil)
		resp, err := m.GenerateStructured(context.Background(), baseReq)
		if err != nil {
			t.Fatalf("MockLLMProvider error = %v", err)
		}
		var out map[string]any
		if err := json.Unmarshal(resp.RawJSON, &out); err != nil {
			t.Fatalf("unmarshal mock output: %v", err)
		}
	})
}
