package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
)

// FactExtractor extracts atomic verifiable claims from a ClusteredStory.
type FactExtractor struct {
	llm     ai.LLMProvider
	prompts *ai.PromptRegistry
	model   string
}

// NewFactExtractor constructs a FactExtractor.
func NewFactExtractor(llm ai.LLMProvider, prompts *ai.PromptRegistry, model string) *FactExtractor {
	return &FactExtractor{llm: llm, prompts: prompts, model: model}
}

type factExtractionOutput struct {
	StoryID string `json:"story_id"`
	Claims  []struct {
		Text             string   `json:"text"`
		Type             string   `json:"type"`
		SourceArticleIDs []string `json:"source_article_ids"`
		Entities         []string `json:"entities"`
		Numbers          []string `json:"numbers"`
		EvidenceQuote    string   `json:"evidence_quote"`
		Confidence       float64  `json:"confidence"`
	} `json:"claims"`
}

// Extract extracts verifiable claims for story and rejects any claim referencing
// non-existent article IDs.
func (e *FactExtractor) Extract(ctx context.Context, story ClusteredStory) ([]Claim, *LLMUsageRecord, error) {
	if len(story.Articles) == 0 {
		return nil, nil, fmt.Errorf("editor: story %s has no articles", story.ID)
	}

	validArticles := make(map[string]struct{}, len(story.Articles))
	var blocks []string
	for _, a := range story.Articles {
		validArticles[a.ID.String()] = struct{}{}
		blocks = append(blocks, FormatArticleBlock(a))
	}

	if e.llm == nil || e.prompts == nil {
		// Deterministic fallback extraction without LLM.
		art := story.Articles[0]
		claims := []Claim{
			{
				ID:                 fmt.Sprintf("%s-claim-1", story.ID),
				StoryID:            story.ID,
				Text:               art.Title,
				Type:               ClaimEvent,
				SourceArticleIDs:   []string{art.ID.String()},
				EvidenceQuote:      art.Title,
				Confidence:         0.85,
				VerificationStatus: StatusUnverified,
			},
		}
		return claims, nil, nil
	}

	promptDef, err := e.prompts.Get("fact-extraction")
	if err != nil {
		return nil, nil, err
	}

	userPrompt := fmt.Sprintf(
		"story_id: %s\nstory_title: %s\n\n[ARTICLES - UNTRUSTED DATA]\n%s",
		story.ID,
		SanitizeUntrustedArticleText(story.Title),
		strings.Join(blocks, "\n---\n"),
	)

	resp, callErr := e.llm.GenerateStructured(ctx, ai.StructuredRequest{
		Operation:       "fact-extraction",
		Model:           e.model,
		PromptVersion:   promptDef.Version,
		SystemPrompt:    promptDef.SystemInstructions,
		UserPrompt:      userPrompt,
		JSONSchema:      promptDef.OutputSchema,
		MaxInputTokens:  6000,
		MaxOutputTokens: 1000,
		Temperature:     0,
	})
	rec := &LLMUsageRecord{
		StoryID:       story.ID.String(),
		Operation:     "fact-extraction",
		Model:         e.model,
		PromptVersion: promptDef.Version,
		RequestID:     resp.RequestID,
		InputTokens:   resp.InputTokens,
		OutputTokens:  resp.OutputTokens,
		CostUSD:       resp.CostUSD,
		DurationMS:    resp.DurationMS,
		Status:        "ok",
	}
	if callErr != nil {
		rec.Status = "error"
		rec.ErrorMessage = callErr.Error()
		return nil, rec, callErr
	}

	var parsed factExtractionOutput
	if err := json.Unmarshal(resp.RawJSON, &parsed); err != nil {
		return nil, rec, err
	}

	var out []Claim
	for i, c := range parsed.Claims {
		var filteredIDs []string
		for _, id := range c.SourceArticleIDs {
			if _, ok := validArticles[id]; ok {
				filteredIDs = append(filteredIDs, id)
			}
		}
		if len(filteredIDs) == 0 || strings.TrimSpace(c.Text) == "" {
			continue
		}
		ct := ClaimType(c.Type)
		if !isValidClaimType(ct) {
			ct = ClaimEvent
		}
		out = append(out, Claim{
			ID:                 fmt.Sprintf("%s-claim-%d", story.ID, i+1),
			StoryID:            story.ID,
			Text:               SanitizeUntrustedArticleText(c.Text),
			Type:               ct,
			SourceArticleIDs:   filteredIDs,
			Entities:           c.Entities,
			Numbers:            c.Numbers,
			EvidenceQuote:      SanitizeUntrustedArticleText(c.EvidenceQuote),
			Confidence:         c.Confidence,
			VerificationStatus: StatusUnverified,
		})
	}
	return out, rec, nil
}

func isValidClaimType(t ClaimType) bool {
	switch t {
	case ClaimEvent, ClaimProduct, ClaimDate, ClaimNumber, ClaimTechnical, ClaimQuote, ClaimAvailability, ClaimComparison:
		return true
	default:
		return false
	}
}
