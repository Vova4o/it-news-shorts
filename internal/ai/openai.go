package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
)

// OpenAIProvider calls an OpenAI-compatible /chat/completions endpoint with
// JSON Schema structured outputs, bounded concurrency, retries, and budget control.
type OpenAIProvider struct {
	cfg        config.LLMConfig
	client     *http.Client
	sem        chan struct{}
	budget     *BudgetTracker
	logger     *slog.Logger
	maxRetries int
	baseWait   time.Duration
}

// NewOpenAIProvider constructs an OpenAI-compatible LLMProvider.
func NewOpenAIProvider(cfg config.LLMConfig, budget *BudgetTracker, logger *slog.Logger) *OpenAIProvider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	conc := cfg.MaxConcurrentRequests
	if conc <= 0 {
		conc = 3
	}
	return &OpenAIProvider{
		cfg:        cfg,
		client:     &http.Client{Timeout: timeout},
		sem:        make(chan struct{}, conc),
		budget:     budget,
		logger:     logger,
		maxRetries: 2,
		baseWait:   500 * time.Millisecond,
	}
}

type openAIChatRequest struct {
	Model          string                `json:"model"`
	Messages       []openAIMessage       `json:"messages"`
	Temperature    float64               `json:"temperature"`
	MaxTokens      int                   `json:"max_tokens,omitempty"`
	ResponseFormat *openAIResponseFormat `json:"response_format,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponseFormat struct {
	Type       string            `json:"type"`
	JSONSchema *openAIJSONSchema `json:"json_schema,omitempty"`
}

type openAIJSONSchema struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type openAIChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// GenerateStructured calls /chat/completions and validates the returned JSON
// against request.JSONSchema.
func (p *OpenAIProvider) GenerateStructured(ctx context.Context, req StructuredRequest) (StructuredResponse, error) {
	if strings.TrimSpace(p.cfg.APIKey) == "" {
		return StructuredResponse{}, fmt.Errorf("%w: LLM_API_KEY is not configured", ErrLLMAuth)
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = p.cfg.Model
	}
	estInputTokens := (len(req.SystemPrompt) + len(req.UserPrompt)) / 4
	if req.MaxInputTokens > 0 && estInputTokens > req.MaxInputTokens {
		return StructuredResponse{}, fmt.Errorf("%w: estimated %d > max %d", ErrLLMTokenLimitExceeded, estInputTokens, req.MaxInputTokens)
	}

	estCost := p.calculateCost(estInputTokens, req.MaxOutputTokens)
	if err := p.budget.ReserveOrCheck(estCost); err != nil {
		return StructuredResponse{}, err
	}

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return StructuredResponse{}, ctx.Err()
	}

	start := time.Now()
	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return StructuredResponse{}, err
		}

		resp, retryAfter, err := p.doOnce(ctx, model, req)
		if err == nil {
			resp.DurationMS = time.Since(start).Milliseconds()
			resp.CostUSD = p.calculateCost(resp.InputTokens, resp.OutputTokens)
			p.budget.Record(resp.CostUSD)
			if p.logger != nil {
				p.logger.Info("llm structured call completed",
					"operation", req.Operation,
					"model", resp.Model,
					"prompt_version", req.PromptVersion,
					"request_id", resp.RequestID,
					"input_tokens", resp.InputTokens,
					"output_tokens", resp.OutputTokens,
					"cost", resp.CostUSD,
					"duration_ms", resp.DurationMS,
					"status", "ok",
				)
			}
			return resp, nil
		}
		lastErr = err
		if !isRetryableLLMError(err) || attempt == p.maxRetries {
			break
		}

		wait := retryAfter
		if wait <= 0 {
			wait = p.baseWait * (1 << attempt)
			wait += time.Duration(rand.Int63n(int64(wait/4) + 1))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return StructuredResponse{}, ctx.Err()
		case <-timer.C:
		}
	}

	if p.logger != nil {
		p.logger.Warn("llm structured call failed",
			"operation", req.Operation,
			"model", model,
			"prompt_version", req.PromptVersion,
			"duration_ms", time.Since(start).Milliseconds(),
			"status", "error",
			"error", lastErr.Error(),
		)
	}
	return StructuredResponse{}, lastErr
}

func (p *OpenAIProvider) doOnce(ctx context.Context, model string, req StructuredRequest) (StructuredResponse, time.Duration, error) {
	payload := openAIChatRequest{
		Model: model,
		Messages: []openAIMessage{
			{Role: "system", Content: req.SystemPrompt},
			{Role: "user", Content: req.UserPrompt},
		},
		Temperature: req.Temperature,
		MaxTokens:   req.MaxOutputTokens,
	}
	if len(req.JSONSchema) > 0 {
		payload.ResponseFormat = &openAIResponseFormat{
			Type: "json_schema",
			JSONSchema: &openAIJSONSchema{
				Name:   strings.ReplaceAll(req.Operation, "-", "_") + "_output",
				Strict: true,
				Schema: req.JSONSchema,
			},
		}
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return StructuredResponse{}, 0, fmt.Errorf("ai: marshal request: %w", err)
	}

	endpoint := strings.TrimRight(p.cfg.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return StructuredResponse{}, 0, fmt.Errorf("ai: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return StructuredResponse{}, 0, fmt.Errorf("%w: %v", ErrLLMTimeout, err)
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return StructuredResponse{}, 0, fmt.Errorf("%w: %v", ErrLLMTimeout, err)
		}
		return StructuredResponse{}, 0, fmt.Errorf("%w: %v", ErrLLMUpstream, err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, 4<<20))
	if err != nil {
		return StructuredResponse{}, 0, fmt.Errorf("%w: read body: %v", ErrLLMUpstream, err)
	}

	if httpResp.StatusCode == http.StatusUnauthorized || httpResp.StatusCode == http.StatusForbidden {
		return StructuredResponse{}, 0, fmt.Errorf("%w: status %d", ErrLLMAuth, httpResp.StatusCode)
	}
	if httpResp.StatusCode == http.StatusTooManyRequests {
		ra := parseRetryAfterSeconds(httpResp.Header.Get("Retry-After"))
		return StructuredResponse{}, ra, fmt.Errorf("%w: status 429", ErrLLMRateLimited)
	}
	if httpResp.StatusCode >= 500 {
		return StructuredResponse{}, 0, fmt.Errorf("%w: status %d", ErrLLMUpstream, httpResp.StatusCode)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return StructuredResponse{}, 0, fmt.Errorf("ai: unexpected status %d", httpResp.StatusCode)
	}

	var parsed openAIChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return StructuredResponse{}, 0, fmt.Errorf("%w: envelope decode: %v", ErrLLMInvalidJSON, err)
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return StructuredResponse{}, 0, ErrLLMEmptyResponse
	}

	rawJSON := json.RawMessage(strings.TrimSpace(parsed.Choices[0].Message.Content))
	if err := ValidateAgainstSchema(rawJSON, req.JSONSchema); err != nil {
		return StructuredResponse{}, 0, err
	}

	usedModel := parsed.Model
	if usedModel == "" {
		usedModel = model
	}

	return StructuredResponse{
		RequestID:     parsed.ID,
		Model:         usedModel,
		PromptVersion: req.PromptVersion,
		RawJSON:       rawJSON,
		InputTokens:   parsed.Usage.PromptTokens,
		OutputTokens:  parsed.Usage.CompletionTokens,
	}, 0, nil
}

func (p *OpenAIProvider) calculateCost(inputTokens, outputTokens int) float64 {
	inCost := float64(inputTokens) / 1_000_000.0 * p.cfg.InputCostPer1MUSD
	outCost := float64(outputTokens) / 1_000_000.0 * p.cfg.OutputCostPer1MUSD
	return inCost + outCost
}

func isRetryableLLMError(err error) bool {
	return errors.Is(err, ErrLLMRateLimited) || errors.Is(err, ErrLLMUpstream) || errors.Is(err, ErrLLMTimeout)
}

func parseRetryAfterSeconds(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
