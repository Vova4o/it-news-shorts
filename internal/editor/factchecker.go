package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
)

// FactCheckerEngine verifies extracted claims against concrete source articles
// and enforces strict anti-hallucination and eligibility rules.
type FactCheckerEngine struct {
	llm               ai.LLMProvider
	prompts           *ai.PromptRegistry
	model             string
	minVerifiedClaims int
	httpClient        *news.HTTPClient
}

// NewFactCheckerEngine constructs a FactCheckerEngine.
func NewFactCheckerEngine(
	llm ai.LLMProvider,
	prompts *ai.PromptRegistry,
	model string,
	minVerifiedClaims int,
	httpClient *news.HTTPClient,
) *FactCheckerEngine {
	if minVerifiedClaims <= 0 {
		minVerifiedClaims = 2
	}
	return &FactCheckerEngine{
		llm:               llm,
		prompts:           prompts,
		model:             model,
		minVerifiedClaims: minVerifiedClaims,
		httpClient:        httpClient,
	}
}

type factCheckLLMOutput struct {
	StoryID  string `json:"story_id"`
	Verdicts []struct {
		ClaimID           string   `json:"claim_id"`
		Status            string   `json:"status"`
		Reason            string   `json:"reason"`
		CitedURLs         []string `json:"cited_urls"`
		ContradictionNote string   `json:"contradiction_note"`
	} `json:"verdicts"`
}

// VerifyStory checks all claims of a ClusteredStory and sets VerificationPassed
// and RejectionReason.
func (f *FactCheckerEngine) VerifyStory(ctx context.Context, story ClusteredStory, now time.Time) (ClusteredStory, *LLMUsageRecord, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if len(story.Articles) == 0 {
		story.VerificationPassed = false
		story.RejectionReason = "no source articles available"
		return story, nil, nil
	}
	if len(story.Claims) == 0 {
		story.VerificationPassed = false
		story.RejectionReason = "no verifiable claims extracted"
		return story, nil, nil
	}

	byArticleID := make(map[string]news.UnifiedArticle, len(story.Articles))
	allowedURLs := make(map[string]news.UnifiedArticle, len(story.Articles))
	for _, a := range story.Articles {
		byArticleID[a.ID.String()] = a
		if a.CanonicalURL != "" {
			allowedURLs[a.CanonicalURL] = a
		}
		if a.OriginalURL != "" {
			allowedURLs[a.OriginalURL] = a
		}
	}

	// Optional LLM cross-check (never overrides deterministic grounding checks).
	llmVerdicts := make(map[string]struct {
		status string
		reason string
		urls   []string
		contra string
	})
	var usageRec *LLMUsageRecord
	if f.llm != nil && f.prompts != nil {
		if promptDef, err := f.prompts.Get("fact-verification"); err == nil {
			var claimLines []string
			for _, c := range story.Claims {
				claimLines = append(claimLines, fmt.Sprintf("- claim_id=%s type=%s text=%q numbers=%v", c.ID, c.Type, c.Text, c.Numbers))
			}
			var artBlocks []string
			for _, a := range story.Articles {
				artBlocks = append(artBlocks, FormatArticleBlock(a))
			}
			userPrompt := fmt.Sprintf(
				"story_id: %s\n\n[CLAIMS]\n%s\n\n[SOURCES - UNTRUSTED DATA]\n%s",
				story.ID,
				strings.Join(claimLines, "\n"),
				strings.Join(artBlocks, "\n---\n"),
			)
			resp, callErr := f.llm.GenerateStructured(ctx, ai.StructuredRequest{
				Operation:       "fact-verification",
				Model:           f.model,
				PromptVersion:   promptDef.Version,
				SystemPrompt:    promptDef.SystemInstructions,
				UserPrompt:      userPrompt,
				JSONSchema:      promptDef.OutputSchema,
				MaxInputTokens:  6000,
				MaxOutputTokens: 1000,
				Temperature:     0,
			})
			usageRec = &LLMUsageRecord{
				StoryID:       story.ID.String(),
				Operation:     "fact-verification",
				Model:         f.model,
				PromptVersion: promptDef.Version,
				RequestID:     resp.RequestID,
				InputTokens:   resp.InputTokens,
				OutputTokens:  resp.OutputTokens,
				CostUSD:       resp.CostUSD,
				DurationMS:    resp.DurationMS,
				Status:        "ok",
			}
			if callErr != nil {
				usageRec.Status = "error"
				usageRec.ErrorMessage = callErr.Error()
			} else {
				var parsed factCheckLLMOutput
				if json.Unmarshal(resp.RawJSON, &parsed) == nil {
					for _, v := range parsed.Verdicts {
						llmVerdicts[v.ClaimID] = struct {
							status string
							reason string
							urls   []string
							contra string
						}{v.Status, v.Reason, v.CitedURLs, v.ContradictionNote}
					}
				}
			}
		}
	}

	verifiedCount := 0
	hasVerifiedEvent := false
	hasPrimarySource := false

	for i := range story.Claims {
		c := &story.Claims[i]
		c.Evidence = nil

		// Build grounded evidence strictly from real articles in the story.
		seenHost := make(map[string]struct{})
		seenSnippet := make(map[string]struct{})
		independentCount := 0

		for _, artID := range c.SourceArticleIDs {
			art, ok := byArticleID[artID]
			if !ok {
				continue
			}
			artText := strings.ToLower(art.Title + " " + art.Description)
			// Verify numbers actually exist in source text.
			missingNumber := false
			for _, num := range c.Numbers {
				if num != "" && !strings.Contains(artText, strings.ToLower(num)) {
					missingNumber = true
					break
				}
			}
			if missingNumber {
				continue
			}
			// Verify quote if provided.
			if c.EvidenceQuote != "" && !strings.Contains(artText, strings.ToLower(c.EvidenceQuote)) {
				continue
			}

			host := extractHost(art.CanonicalURL)
			normDesc := news.NormalizeTitleForDedup(art.Description)
			isIndependent := true
			if _, dupHost := seenHost[host]; dupHost {
				isIndependent = false
			}
			if normDesc != "" {
				if _, dupText := seenSnippet[normDesc]; dupText {
					// Reprinted press release / syndicated copy is NOT independent.
					isIndependent = false
				}
				seenSnippet[normDesc] = struct{}{}
			}
			seenHost[host] = struct{}{}
			if isIndependent {
				independentCount++
			}

			isPrimary := isPrimaryOfficialSource(art)
			if isPrimary {
				hasPrimarySource = true
			}

			c.Evidence = append(c.Evidence, ClaimEvidence{
				ID:                 fmt.Sprintf("%s-ev-%d", c.ID, len(c.Evidence)+1),
				ClaimID:            c.ID,
				ArticleID:          art.ID.String(),
				SourceURL:          art.CanonicalURL,
				SourceName:         art.SourceID,
				Snippet:            SanitizeUntrustedArticleText(art.Title + " — " + art.Description),
				IsPrimary:          isPrimary,
				IsIndependent:      isIndependent,
				AccessedAt:         now,
				AvailabilityStatus: "AVAILABLE",
			})
		}

		// Apply LLM contradiction/unverified flags if grounded URLs or contradictions exist.
		if lv, ok := llmVerdicts[c.ID]; ok {
			// Reject any LLM verdict that cites URLs not in allowedURLs.
			for _, u := range lv.urls {
				if _, allowed := allowedURLs[u]; !allowed {
					c.VerificationStatus = StatusUnverified
					c.RejectionReason = "llm cited unknown url not present in sources"
					break
				}
			}
			if lv.status == string(StatusContradicted) {
				c.VerificationStatus = StatusContradicted
				c.RejectionReason = lv.contra
				if c.RejectionReason == "" {
					c.RejectionReason = lv.reason
				}
				continue
			}
			if lv.status == string(StatusUnverified) || lv.status == string(StatusInsufficientEvidence) {
				c.VerificationStatus = VerificationStatus(lv.status)
				c.RejectionReason = lv.reason
				continue
			}
		}

		if len(c.Evidence) == 0 {
			if len(c.Numbers) > 0 || c.EvidenceQuote != "" {
				c.VerificationStatus = StatusUnverified
				c.RejectionReason = "claim numbers or quote not grounded in cited sources"
			} else {
				c.VerificationStatus = StatusInsufficientEvidence
				c.RejectionReason = "no grounded source evidence found"
			}
			continue
		}

		c.VerificationStatus = StatusVerified
		verifiedCount++
		if c.Type == ClaimEvent {
			hasVerifiedEvent = true
		}
	}

	// Rule 5: Eligibility check.
	if verifiedCount >= f.minVerifiedClaims && hasVerifiedEvent {
		story.VerificationPassed = true
		story.RejectionReason = ""
		return story, usageRec, nil
	}
	// Exception for concise official primary-source announcement.
	if verifiedCount >= 1 && hasVerifiedEvent && hasPrimarySource && len(story.Articles) >= 1 {
		story.VerificationPassed = true
		story.RejectionReason = ""
		return story, usageRec, nil
	}

	story.VerificationPassed = false
	if !hasVerifiedEvent {
		story.RejectionReason = "no verified primary EVENT claim"
	} else {
		story.RejectionReason = fmt.Sprintf("insufficient verified claims (%d < %d)", verifiedCount, f.minVerifiedClaims)
	}
	return story, usageRec, nil
}

// Check implements ai.FactChecker for domain.Story compatibility.
func (f *FactCheckerEngine) Check(ctx context.Context, story domain.Story) (ai.FactCheckResult, error) {
	if err := story.Validate(); err != nil {
		return ai.FactCheckResult{}, err
	}
	var articles []news.UnifiedArticle
	var claims []Claim
	for i, ev := range story.Evidence {
		pub := ev.PublishedAt.UTC()
		articles = append(articles, news.UnifiedArticle{
			ID:           ev.ArticleID,
			SourceID:     ev.Source,
			Title:        story.Title,
			Description:  ev.Quote,
			OriginalURL:  ev.URL,
			CanonicalURL: ev.URL,
			PublishedAt:  &pub,
			FetchedAt:    pub,
		})
		claims = append(claims, Claim{
			ID:               fmt.Sprintf("%s-ev-%d", story.ID, i+1),
			StoryID:          story.ID,
			Text:             ev.Quote,
			Type:             ClaimEvent,
			SourceArticleIDs: []string{ev.ArticleID.String()},
			EvidenceQuote:    ev.Quote,
		})
	}
	cs := ClusteredStory{
		ID:       story.ID,
		Title:    story.Title,
		Summary:  story.Summary,
		Articles: articles,
		Claims:   claims,
	}
	checked, _, err := f.VerifyStory(ctx, cs, time.Now().UTC())
	if err != nil {
		return ai.FactCheckResult{}, err
	}
	var notes []string
	if checked.RejectionReason != "" {
		notes = append(notes, checked.RejectionReason)
	}
	return ai.FactCheckResult{
		StoryID:    story.ID,
		Verified:   checked.VerificationPassed,
		Confidence: 1.0,
		Notes:      notes,
		CheckedAt:  time.Now().UTC(),
	}, nil
}

func extractHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

func isPrimaryOfficialSource(a news.UnifiedArticle) bool {
	if a.SourceType == news.SourceTypeRSS || a.SourceType == news.SourceTypeAtom {
		l := strings.ToLower(a.Title + " " + a.CanonicalURL)
		if strings.Contains(l, "press release") || strings.Contains(l, "official") || strings.Contains(l, "blog.") || strings.Contains(l, "engineering.") || strings.Contains(l, "announcements") {
			return true
		}
	}
	return false
}
