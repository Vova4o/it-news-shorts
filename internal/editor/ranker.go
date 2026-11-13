package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// Scoring weights from TASK 005 specification.
const (
	WeightImpact    = 0.35
	WeightInterest  = 0.25
	WeightFreshness = 0.20
	WeightNovelty   = 0.20
)

// CalculateFinalScore deterministically computes the weighted 0-100 score.
func CalculateFinalScore(impact, interest, freshness, novelty float64) (float64, error) {
	for name, val := range map[string]float64{
		"impact":    impact,
		"interest":  interest,
		"freshness": freshness,
		"novelty":   novelty,
	} {
		if math.IsNaN(val) || math.IsInf(val, 0) || val < 0 || val > 100 {
			return 0, fmt.Errorf("editor: invalid %s score %v (must be in [0, 100])", name, val)
		}
	}
	raw := impact*WeightImpact + interest*WeightInterest + freshness*WeightFreshness + novelty*WeightNovelty
	return math.Round(raw*100) / 100, nil
}

// RecalculateScore recomputes FinalScore from stored sub-scores.
func RecalculateScore(s StoryScore) (StoryScore, error) {
	final, err := CalculateFinalScore(s.Impact, s.Interest, s.Freshness, s.Novelty)
	if err != nil {
		return StoryScore{}, err
	}
	s.FinalScore = final
	return s, nil
}

// StoryRankerEngine evaluates verified stories using LLM sub-scores and a
// deterministic Go weighting formula.
type StoryRankerEngine struct {
	llm      ai.LLMProvider
	prompts  *ai.PromptRegistry
	model    string
	language string
}

// NewStoryRankerEngine constructs a StoryRankerEngine.
func NewStoryRankerEngine(llm ai.LLMProvider, prompts *ai.PromptRegistry, model, language string) *StoryRankerEngine {
	if strings.TrimSpace(language) == "" {
		language = "ru"
	}
	return &StoryRankerEngine{
		llm:      llm,
		prompts:  prompts,
		model:    model,
		language: language,
	}
}

type scoringLLMOutput struct {
	StoryID          string  `json:"story_id"`
	Impact           float64 `json:"impact"`
	Interest         float64 `json:"interest"`
	Freshness        float64 `json:"freshness"`
	Novelty          float64 `json:"novelty"`
	ReasoningSummary string  `json:"reasoning_summary"`
	Category         string  `json:"category"`
}

// ScoreStory evaluates one verified ClusteredStory.
func (r *StoryRankerEngine) ScoreStory(ctx context.Context, story ClusteredStory, now time.Time) (ClusteredStory, *LLMUsageRecord, error) {
	if !story.VerificationPassed {
		return story, nil, fmt.Errorf("editor: cannot score unverified story %s", story.ID)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	if r.llm == nil || r.prompts == nil {
		score, err := r.heuristicScore(story, now)
		if err != nil {
			return story, nil, err
		}
		story.Score = &score
		return story, nil, nil
	}

	promptDef, err := r.prompts.Get("scoring")
	if err != nil {
		return story, nil, err
	}

	var verifiedClaims []string
	for _, c := range story.Claims {
		if c.VerificationStatus == StatusVerified {
			verifiedClaims = append(verifiedClaims, fmt.Sprintf("- [%s] %s", c.Type, c.Text))
		}
	}

	freshnessNote := "unknown"
	for _, a := range story.Articles {
		if a.PublishedAt != nil {
			freshnessNote = a.PublishedAt.UTC().Format(time.RFC3339)
			break
		}
	}

	userPrompt := fmt.Sprintf(
		"story_id: %s\ntarget_language: %s\ntitle: %s\noriginal_published_at: %s\nsource_count: %d\n\n[VERIFIED CLAIMS]\n%s",
		story.ID,
		r.language,
		SanitizeUntrustedArticleText(story.Title),
		freshnessNote,
		len(story.Articles),
		strings.Join(verifiedClaims, "\n"),
	)

	resp, callErr := r.llm.GenerateStructured(ctx, ai.StructuredRequest{
		Operation:       "scoring",
		Model:           r.model,
		PromptVersion:   promptDef.Version,
		SystemPrompt:    promptDef.SystemInstructions,
		UserPrompt:      userPrompt,
		JSONSchema:      promptDef.OutputSchema,
		MaxInputTokens:  4000,
		MaxOutputTokens: 400,
		Temperature:     0,
	})
	rec := &LLMUsageRecord{
		StoryID:       story.ID.String(),
		Operation:     "scoring",
		Model:         r.model,
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
		return story, rec, callErr
	}

	var out scoringLLMOutput
	if err := json.Unmarshal(resp.RawJSON, &out); err != nil {
		return story, rec, err
	}

	final, err := CalculateFinalScore(out.Impact, out.Interest, out.Freshness, out.Novelty)
	if err != nil {
		return story, rec, err
	}
	if strings.TrimSpace(out.ReasoningSummary) == "" {
		return story, rec, fmt.Errorf("editor: empty reasoning_summary from LLM")
	}

	if cat := StoryCategory(out.Category); cat.Valid() {
		story.Category = cat
	}
	story.Score = &StoryScore{
		StoryID:          story.ID,
		Impact:           out.Impact,
		Interest:         out.Interest,
		Freshness:        out.Freshness,
		Novelty:          out.Novelty,
		FinalScore:       final,
		ReasoningSummary: out.ReasoningSummary,
		ModelVersion:     resp.Model,
		PromptVersion:    promptDef.Version,
		CalculatedAt:     now,
	}
	return story, rec, nil
}

func (r *StoryRankerEngine) heuristicScore(story ClusteredStory, now time.Time) (StoryScore, error) {
	impact := 65.0
	if len(story.Articles) > 1 {
		impact += 10.0
	}
	interest := 60.0
	for _, a := range story.Articles {
		if a.PopularityScore != nil && *a.PopularityScore > 100 {
			interest = 80.0
			break
		}
	}
	freshness := 70.0
	for _, a := range story.Articles {
		if a.PublishedAt != nil {
			ageHours := now.Sub(a.PublishedAt.UTC()).Hours()
			if ageHours <= 6 {
				freshness = 95.0
			} else if ageHours <= 12 {
				freshness = 85.0
			} else if ageHours <= 24 {
				freshness = 75.0
			} else {
				freshness = 40.0
			}
			break
		}
	}
	novelty := 75.0
	final, err := CalculateFinalScore(impact, interest, freshness, novelty)
	if err != nil {
		return StoryScore{}, err
	}
	return StoryScore{
		StoryID:          story.ID,
		Impact:           impact,
		Interest:         interest,
		Freshness:        freshness,
		Novelty:          novelty,
		FinalScore:       final,
		ReasoningSummary: "Deterministic fallback editorial score based on source verification and freshness.",
		ModelVersion:     "heuristic-v1",
		PromptVersion:    "heuristic-v1",
		CalculatedAt:     now,
	}, nil
}

// Rank implements ai.StoryRanker for domain.Story compatibility.
func (r *StoryRankerEngine) Rank(ctx context.Context, candidates []domain.Story) ([]ai.RankedStory, error) {
	now := time.Now().UTC()
	out := make([]ai.RankedStory, 0, len(candidates))
	for _, s := range candidates {
		cs := ClusteredStory{
			ID:                 s.ID,
			Title:              s.Title,
			Summary:            s.Summary,
			Category:           InferCategory(s.Title),
			VerificationPassed: true,
			Claims: []Claim{
				{
					ID:                 s.ID.String() + "-c1",
					StoryID:            s.ID,
					Text:               s.Title,
					Type:               ClaimEvent,
					VerificationStatus: StatusVerified,
				},
			},
			CreatedAt: s.CreatedAt,
		}
		scored, _, err := r.ScoreStory(ctx, cs, now)
		if err != nil {
			return nil, err
		}
		out = append(out, ai.RankedStory{
			StoryID: s.ID,
			Score:   scored.Score.FinalScore / 100.0,
			Reason:  scored.Score.ReasoningSummary,
		})
	}
	return out, nil
}
