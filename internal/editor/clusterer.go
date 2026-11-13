package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
)

// StoryClusterer groups articles describing the same real-world IT event using
// cheap heuristic candidate selection followed by LLM semantic verification.
type StoryClusterer struct {
	llm     ai.LLMProvider
	prompts *ai.PromptRegistry
	model   string
}

// NewStoryClusterer constructs a StoryClusterer.
func NewStoryClusterer(llm ai.LLMProvider, prompts *ai.PromptRegistry, model string) *StoryClusterer {
	return &StoryClusterer{llm: llm, prompts: prompts, model: model}
}

type clusterVerifyOutput struct {
	SameEvent      bool    `json:"same_event"`
	Relationship   string  `json:"relationship"`
	Confidence     float64 `json:"confidence"`
	Reason         string  `json:"reason"`
	CanonicalTitle string  `json:"canonical_title"`
	Category       string  `json:"category"`
}

// ClusterResult holds clustered stories and LLM usage telemetry.
type ClusterResult struct {
	Stories []ClusteredStory
	Usage   []LLMUsageRecord
}

// Cluster groups articles into unique ClusteredStory events without losing any
// source articles.
func (c *StoryClusterer) Cluster(ctx context.Context, articles []news.UnifiedArticle, now time.Time) (ClusterResult, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	// Deterministic order before clustering.
	sorted := append([]news.UnifiedArticle(nil), articles...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, tj := effectiveArticleTime(sorted[i]), effectiveArticleTime(sorted[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return sorted[i].ID < sorted[j].ID
	})

	var stories []ClusteredStory
	var usage []LLMUsageRecord

	for _, art := range sorted {
		if err := ctx.Err(); err != nil {
			return ClusterResult{}, err
		}
		merged := false
		for idx := range stories {
			rep := stories[idx].Articles[0]
			if !isHeuristicCandidate(rep, art) {
				continue
			}

			same, isUpdate, conf, reason, canonTitle, cat, rec, err := c.verifyPair(ctx, rep, art)
			if rec != nil {
				usage = append(usage, *rec)
			}
			if err != nil {
				// Fail closed on semantic uncertainty: keep separate rather than false merge.
				continue
			}
			if !same {
				continue
			}

			role := "CORROBORATING"
			if isUpdate {
				role = "UPDATE"
				stories[idx].IsMultiDayUpdate = true
				stories[idx].IsSignificantUpdate = true
			}
			stories[idx].Articles = append(stories[idx].Articles, art)
			stories[idx].ArticleLinks = append(stories[idx].ArticleLinks, StoryArticleLink{
				ArticleID:  art.ID.String(),
				Role:       role,
				Confidence: conf,
				Reason:     reason,
			})
			if canonTitle != "" && len(stories[idx].Articles) == 2 {
				stories[idx].Title = canonTitle
			}
			if cat.Valid() && stories[idx].Category == CategoryOtherIT {
				stories[idx].Category = cat
			}
			merged = true
			break
		}

		if !merged {
			cat := InferCategory(art.Title + " " + art.Description)
			fp := StoryFingerprint(art.Title)
			stories = append(stories, ClusteredStory{
				ID:            DeterministicStoryID(fp),
				Title:         art.Title,
				Summary:       art.Description,
				Category:      cat,
				Fingerprint:   fp,
				Confidence:    1.0,
				ClusterReason: "Single-article event cluster",
				Articles:      []news.UnifiedArticle{art},
				ArticleLinks: []StoryArticleLink{
					{
						ArticleID:  art.ID.String(),
						Role:       "PRIMARY",
						Confidence: 1.0,
						Reason:     "Initial seed article",
					},
				},
				CreatedAt: now,
			})
		}
	}

	return ClusterResult{Stories: stories, Usage: usage}, nil
}

func (c *StoryClusterer) verifyPair(
	ctx context.Context,
	a, b news.UnifiedArticle,
) (same bool, isUpdate bool, conf float64, reason string, canonTitle string, cat StoryCategory, rec *LLMUsageRecord, err error) {
	if a.CanonicalURL != "" && a.CanonicalURL == b.CanonicalURL {
		return true, false, 1.0, "Identical canonical URL", a.Title, InferCategory(a.Title), nil, nil
	}

	if c.llm == nil || c.prompts == nil {
		sim := news.TitleSimilarity(a.Title, b.Title)
		if sim >= 0.55 {
			return true, isMultiDayUpdate(a, b), sim, fmt.Sprintf("Heuristic title similarity %.2f", sim), a.Title, InferCategory(a.Title), nil, nil
		}
		return false, false, sim, "Insufficient heuristic similarity", "", CategoryOtherIT, nil, nil
	}

	promptDef, err := c.prompts.Get("clustering")
	if err != nil {
		return false, false, 0, "", "", CategoryOtherIT, nil, err
	}

	userPrompt := fmt.Sprintf(
		"Determine whether Article A and Article B describe the same event.\n\n[ARTICLE A - UNTRUSTED DATA]\n%s\n\n[ARTICLE B - UNTRUSTED DATA]\n%s",
		FormatArticleBlock(a),
		FormatArticleBlock(b),
	)

	resp, callErr := c.llm.GenerateStructured(ctx, ai.StructuredRequest{
		Operation:       "clustering",
		Model:           c.model,
		PromptVersion:   promptDef.Version,
		SystemPrompt:    promptDef.SystemInstructions,
		UserPrompt:      userPrompt,
		JSONSchema:      promptDef.OutputSchema,
		MaxInputTokens:  4000,
		MaxOutputTokens: 300,
		Temperature:     0,
	})
	usageRec := &LLMUsageRecord{
		Operation:     "clustering",
		Model:         c.model,
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
		return false, false, 0, "", "", CategoryOtherIT, usageRec, callErr
	}

	var out clusterVerifyOutput
	if err := json.Unmarshal(resp.RawJSON, &out); err != nil {
		return false, false, 0, "", "", CategoryOtherIT, usageRec, err
	}

	parsedCat := StoryCategory(out.Category)
	if !parsedCat.Valid() {
		parsedCat = InferCategory(a.Title)
	}
	isUpd := out.Relationship == "UPDATE" || (out.SameEvent && isMultiDayUpdate(a, b))
	return out.SameEvent && out.Relationship != "DIFFERENT_EVENTS", isUpd, out.Confidence, out.Reason, out.CanonicalTitle, parsedCat, usageRec, nil
}

var (
	securityKwRe = regexp.MustCompile(`(?i)\b(cve-\d{4}-\d+|vulnerability|exploit|zero-day|0-day|breach|ransomware|flaw|security patch|leak)\b`)
	rumorKwRe    = regexp.MustCompile(`(?i)\b(rumor|rumour|leaked|reportedly|expected to|might|could|forecast|predicted|speculation)\b`)
	versionRe    = regexp.MustCompile(`(?i)\b(v?\d+(?:\.\d+)?)\b`)
)

func isHeuristicCandidate(a, b news.UnifiedArticle) bool {
	if a.CanonicalURL != "" && a.CanonicalURL == b.CanonicalURL {
		return true
	}

	// Temporal proximity check: within 72 hours.
	ta, tb := effectiveArticleTime(a), effectiveArticleTime(b)
	diff := ta.Sub(tb)
	if diff < 0 {
		diff = -diff
	}
	if diff > 72*time.Hour {
		return false
	}

	// Guard: do not merge security/vulnerability reports with product launches.
	if securityKwRe.MatchString(a.Title) != securityKwRe.MatchString(b.Title) {
		return false
	}
	// Guard: do not merge rumors/forecasts with confirmed announcements.
	if rumorKwRe.MatchString(a.Title) != rumorKwRe.MatchString(b.Title) {
		return false
	}
	// Guard: do not merge distinct explicit version numbers (e.g. GPT-4 vs GPT-5).
	if hasConflictingVersions(a.Title, b.Title) {
		return false
	}

	if news.TitleSimilarity(a.Title, b.Title) >= 0.40 {
		return true
	}
	shared := countSharedEntities(a.Title, b.Title)
	return shared >= 2
}

func hasConflictingVersions(t1, t2 string) bool {
	v1 := versionRe.FindAllString(strings.ToLower(t1), -1)
	v2 := versionRe.FindAllString(strings.ToLower(t2), -1)
	if len(v1) == 0 || len(v2) == 0 {
		return false
	}
	set1 := make(map[string]struct{}, len(v1))
	for _, v := range v1 {
		set1[v] = struct{}{}
	}
	for _, v := range v2 {
		if _, ok := set1[v]; ok {
			return false
		}
	}
	return true
}

func countSharedEntities(t1, t2 string) int {
	e1 := extractEntityTokens(t1)
	e2 := extractEntityTokens(t2)
	count := 0
	for k := range e1 {
		if _, ok := e2[k]; ok {
			count++
		}
	}
	return count
}

func extractEntityTokens(s string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, raw := range strings.Fields(s) {
		clean := strings.TrimFunc(raw, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		if len(clean) < 3 {
			continue
		}
		if unicode.IsUpper(rune(clean[0])) {
			out[strings.ToLower(clean)] = struct{}{}
		}
	}
	return out
}

func effectiveArticleTime(a news.UnifiedArticle) time.Time {
	if a.PublishedAt != nil && !a.PublishedAt.IsZero() {
		return a.PublishedAt.UTC()
	}
	if a.SourceSubmittedAt != nil && !a.SourceSubmittedAt.IsZero() {
		return a.SourceSubmittedAt.UTC()
	}
	return a.FetchedAt.UTC()
}

func isMultiDayUpdate(a, b news.UnifiedArticle) bool {
	diff := effectiveArticleTime(a).Sub(effectiveArticleTime(b))
	if diff < 0 {
		diff = -diff
	}
	return diff >= 18*time.Hour
}

// InferCategory assigns a deterministic StoryCategory based on keywords.
func InferCategory(text string) StoryCategory {
	l := strings.ToLower(text)
	switch {
	case strings.Contains(l, "cve-") || strings.Contains(l, "vulnerability") || strings.Contains(l, "cyber") || strings.Contains(l, "breach") || strings.Contains(l, "ransomware"):
		return CategoryCybersecurity
	case strings.Contains(l, "openai") || strings.Contains(l, "anthropic") || strings.Contains(l, "llm") || strings.Contains(l, "ai ") || strings.Contains(l, "model"):
		return CategoryAI
	case strings.Contains(l, "nvidia") || strings.Contains(l, "amd") || strings.Contains(l, "intel") || strings.Contains(l, "chip") || strings.Contains(l, "gpu") || strings.Contains(l, "cpu"):
		return CategoryHardware
	case strings.Contains(l, "kubernetes") || strings.Contains(l, "aws") || strings.Contains(l, "cloud") || strings.Contains(l, "azure"):
		return CategoryCloud
	case strings.Contains(l, "robot") || strings.Contains(l, "humanoid"):
		return CategoryRobotics
	case strings.Contains(l, "golang") || strings.Contains(l, "rust") || strings.Contains(l, "python") || strings.Contains(l, "compiler") || strings.Contains(l, "github"):
		return CategorySoftwareDev
	case strings.Contains(l, "apple") || strings.Contains(l, "google") || strings.Contains(l, "microsoft") || strings.Contains(l, "meta"):
		return CategoryBigTech
	default:
		return CategoryOtherIT
	}
}
