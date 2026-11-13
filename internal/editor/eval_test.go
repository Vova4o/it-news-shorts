package editor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
)

type evalDataset struct {
	Version        string `json:"version"`
	WindowStart    string `json:"window_start"`
	WindowEnd      string `json:"window_end"`
	ExpectedEvents []struct {
		EventID       string  `json:"event_id"`
		Category      string  `json:"category"`
		Eligible      bool    `json:"eligible"`
		ExpectedScore float64 `json:"expected_score"`
	} `json:"expected_events"`
	Articles []struct {
		ID                string `json:"id"`
		ExpectedEventID   string `json:"expected_event_id"`
		SourceID          string `json:"source_id"`
		Title             string `json:"title"`
		Description       string `json:"description"`
		URL               string `json:"url"`
		PublishedAt       string `json:"published_at"`
		IsOld             bool   `json:"is_old"`
		IsContradicted    bool   `json:"is_contradicted"`
		IsPromptInjection bool   `json:"is_prompt_injection"`
	} `json:"articles"`
}

func TestQualityEvaluationDataset(t *testing.T) {
	rawData, err := os.ReadFile(filepath.Join("..", "..", "testdata", "editor_eval_dataset.json"))
	if err != nil {
		t.Fatalf("read eval dataset: %v", err)
	}
	var ds evalDataset
	if err := json.Unmarshal(rawData, &ds); err != nil {
		t.Fatalf("unmarshal eval dataset: %v", err)
	}
	if len(ds.Articles) < 50 {
		t.Fatalf("expected >= 50 articles in eval dataset, got %d", len(ds.Articles))
	}

	prompts, err := ai.LoadEmbeddedPrompts()
	if err != nil {
		t.Fatalf("LoadEmbeddedPrompts: %v", err)
	}

	windowEnd, err := time.Parse(time.RFC3339, ds.WindowEnd)
	if err != nil {
		t.Fatalf("parse window_end: %v", err)
	}
	windowStart, err := time.Parse(time.RFC3339, ds.WindowStart)
	if err != nil {
		t.Fatalf("parse window_start: %v", err)
	}

	// Build lookup maps from dataset ground truth.
	articleEvent := make(map[string]string, len(ds.Articles))
	articleInfo := make(map[string]struct {
		EventID           string
		Title             string
		Desc              string
		URL               string
		IsOld             bool
		IsContradicted    bool
		IsPromptInjection bool
	}, len(ds.Articles))
	eventMeta := make(map[string]struct {
		Category      string
		Eligible      bool
		ExpectedScore float64
	}, len(ds.ExpectedEvents))
	for _, ev := range ds.ExpectedEvents {
		eventMeta[ev.EventID] = struct {
			Category      string
			Eligible      bool
			ExpectedScore float64
		}{ev.Category, ev.Eligible, ev.ExpectedScore}
	}

	var articles []news.UnifiedArticle
	for _, a := range ds.Articles {
		pub, err := time.Parse(time.RFC3339, a.PublishedAt)
		if err != nil {
			t.Fatalf("parse published_at for %s: %v", a.ID, err)
		}
		articleEvent[a.ID] = a.ExpectedEventID
		articleInfo[a.ID] = struct {
			EventID           string
			Title             string
			Desc              string
			URL               string
			IsOld             bool
			IsContradicted    bool
			IsPromptInjection bool
		}{a.ExpectedEventID, a.Title, a.Description, a.URL, a.IsOld, a.IsContradicted, a.IsPromptInjection}

		// Pre-filter articles outside 24h window like the pipeline preprocessing step.
		if pub.Before(windowStart) {
			continue
		}
		articles = append(articles, makeTestArticle(a.ID, a.SourceID, a.Title, a.Description, a.URL, pub))
	}

	var totalLLMResponses, schemaValidResponses int
	checkSchema := func(op string, raw json.RawMessage) {
		totalLLMResponses++
		tmpl, err := prompts.Get(op)
		if err != nil {
			return
		}
		if err := ai.ValidateAgainstSchema(tmpl.OutputSchema, raw); err == nil {
			schemaValidResponses++
		}
	}

	storyIDRe := regexp.MustCompile(`story-[a-f0-9]+`)
	claimIDRe := regexp.MustCompile(`story-[a-f0-9]+-claim-\d+`)

	mock := ai.NewMockLLMProvider("mock-eval-v1", nil, func(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
		switch req.Operation {
		case "clustering":
			// Determine event IDs of candidate articles from prompt payload.
			var ids []string
			for id := range articleEvent {
				if strings.Contains(req.UserPrompt, id) {
					ids = append(ids, id)
				}
			}
			same := false
			cat := "OTHER_IT"
			title := "Story"
			if len(ids) >= 2 {
				ev0 := articleEvent[ids[0]]
				same = true
				for _, id := range ids[1:] {
					if articleEvent[id] != ev0 {
						same = false
						break
					}
				}
				if meta, ok := eventMeta[ev0]; ok {
					cat = meta.Category
				}
				title = articleInfo[ids[0]].Title
			}
			rel := "DIFFERENT_EVENT"
			conf := 0.2
			reason := "Distinct events"
			if same {
				rel = "SAME_EVENT"
				conf = 0.94
				reason = "Both articles report the exact same underlying event"
			}
			raw, _ := json.Marshal(map[string]any{
				"same_event":      same,
				"relationship":    rel,
				"confidence":      conf,
				"reason":          reason,
				"canonical_title": title,
				"category":        cat,
			})
			checkSchema(req.Operation, raw)
			return ai.StructuredResponse{RequestID: "eval-cluster", Model: "mock-eval-v1", PromptVersion: req.PromptVersion, RawJSON: raw}, nil

		case "fact-extraction":
			storyID := storyIDRe.FindString(req.UserPrompt)
			if storyID == "" {
				storyID = "story-unknown"
			}
			var matchedIDs []string
			for id := range articleInfo {
				if strings.Contains(req.UserPrompt, id) {
					matchedIDs = append(matchedIDs, id)
				}
			}
			var claims []map[string]any
			if len(matchedIDs) > 0 {
				first := articleInfo[matchedIDs[0]]
				claims = append(claims, map[string]any{
					"text":               first.Title,
					"type":               "EVENT",
					"source_article_ids": matchedIDs,
					"entities":           []string{"IT"},
					"numbers":            []string{},
					"evidence_quote":     first.Title,
					"confidence":         0.92,
				})
				claims = append(claims, map[string]any{
					"text":               first.Desc,
					"type":               "TECHNICAL",
					"source_article_ids": matchedIDs,
					"entities":           []string{},
					"numbers":            []string{},
					"evidence_quote":     first.Desc,
					"confidence":         0.90,
				})
			}
			raw, _ := json.Marshal(map[string]any{
				"story_id": storyID,
				"claims":   claims,
			})
			checkSchema(req.Operation, raw)
			return ai.StructuredResponse{RequestID: "eval-extract", Model: "mock-eval-v1", PromptVersion: req.PromptVersion, RawJSON: raw}, nil

		case "fact-verification":
			storyID := storyIDRe.FindString(req.UserPrompt)
			if storyID == "" {
				storyID = "story-unknown"
			}
			claimIDs := claimIDRe.FindAllString(req.UserPrompt, -1)
			seenCID := make(map[string]bool)
			var uniqueCIDs []string
			for _, cid := range claimIDs {
				if !seenCID[cid] {
					seenCID[cid] = true
					uniqueCIDs = append(uniqueCIDs, cid)
				}
			}
			var verdicts []map[string]any
			isContradicted := false
			isInjection := false
			isIneligible := false
			var urls []string
			for id, info := range articleInfo {
				if strings.Contains(req.UserPrompt, id) || strings.Contains(req.UserPrompt, info.URL) {
					urls = append(urls, info.URL)
					if info.IsContradicted {
						isContradicted = true
					}
					if info.IsPromptInjection {
						isInjection = true
					}
					if !eventMeta[info.EventID].Eligible {
						isIneligible = true
					}
				}
			}
			for _, cid := range uniqueCIDs {
				status := "VERIFIED"
				reason := "Confirmed by cited source text"
				note := ""
				if isContradicted {
					status = "CONTRADICTED"
					reason = "Sources disagree on key figures"
					note = "Conflicting numbers across sources"
				} else if isInjection || isIneligible {
					status = "UNVERIFIED"
					reason = "Unsupported rumor, PR fluff, or prompt injection without primary evidence"
				}
				verdicts = append(verdicts, map[string]any{
					"claim_id":           cid,
					"status":             status,
					"reason":             reason,
					"cited_urls":         urls,
					"contradiction_note": note,
				})
			}
			raw, _ := json.Marshal(map[string]any{
				"story_id": storyID,
				"verdicts": verdicts,
			})
			checkSchema(req.Operation, raw)
			return ai.StructuredResponse{RequestID: "eval-verify", Model: "mock-eval-v1", PromptVersion: req.PromptVersion, RawJSON: raw}, nil

		case "scoring":
			storyID := storyIDRe.FindString(req.UserPrompt)
			if storyID == "" {
				storyID = "story-unknown"
			}
			score := 70.0
			cat := "OTHER_IT"
			for id, info := range articleInfo {
				if strings.Contains(req.UserPrompt, id) || strings.Contains(req.UserPrompt, info.Title) {
					if meta, ok := eventMeta[info.EventID]; ok {
						score = meta.ExpectedScore
						cat = meta.Category
					}
					break
				}
			}
			if score <= 0 {
				score = 50.0
			}
			raw, _ := json.Marshal(map[string]any{
				"story_id":          storyID,
				"category":          cat,
				"impact":            score,
				"interest":          score,
				"freshness":         score,
				"novelty":           score,
				"reasoning_summary": fmt.Sprintf("Evaluated score %.1f for %s", score, storyID),
			})
			checkSchema(req.Operation, raw)
			return ai.StructuredResponse{RequestID: "eval-score", Model: "mock-eval-v1", PromptVersion: req.PromptVersion, RawJSON: raw}, nil
		}
		return ai.StructuredResponse{}, fmt.Errorf("unexpected operation %s", req.Operation)
	})

	ctx := context.Background()
	clusterer := NewStoryClusterer(mock, prompts, "mock-eval-v1")
	clusterRes, err := clusterer.Cluster(ctx, articles, windowEnd)
	if err != nil {
		t.Fatalf("Cluster() error = %v", err)
	}

	// Measure clustering precision and false merges.
	var totalPairs, correctPairs, falseMerges int
	for _, st := range clusterRes.Stories {
		if len(st.Articles) < 2 {
			continue
		}
		for i := 0; i < len(st.Articles); i++ {
			for j := i + 1; j < len(st.Articles); j++ {
				totalPairs++
				evI := articleEvent[string(st.Articles[i].ID)]
				evJ := articleEvent[string(st.Articles[j].ID)]
				if evI == evJ {
					correctPairs++
				} else {
					falseMerges++
					t.Errorf("false merge: %s (%s) merged with %s (%s)", st.Articles[i].ID, evI, st.Articles[j].ID, evJ)
				}
			}
		}
	}
	precision := 1.0
	if totalPairs > 0 {
		precision = float64(correctPairs) / float64(totalPairs)
	}
	if precision < 0.90 {
		t.Fatalf("clustering precision = %.2f%%, want >= 90%%", precision*100)
	}
	if falseMerges != 0 {
		t.Fatalf("false merges = %d, want 0", falseMerges)
	}

	// Run extraction, verification, scoring, and selection.
	extractor := NewFactExtractor(mock, prompts, "mock-eval-v1")
	checker := NewFactCheckerEngine(mock, prompts, "mock-eval-v1", 2, nil)
	ranker := NewStoryRankerEngine(mock, prompts, "mock-eval-v1", "v1")
	selector := NewDailyStorySelector(3)

	var verified []ClusteredStory
	verifiedByID := make(map[string]ClusteredStory)
	unverifiedMarkedVerified := 0
	for i := range clusterRes.Stories {
		st := clusterRes.Stories[i]
		claims, _, err := extractor.Extract(ctx, st)
		if err != nil {
			t.Fatalf("Extract(%s) error = %v", st.ID, err)
		}
		st.Claims = claims
		checked, _, err := checker.VerifyStory(ctx, st, windowEnd)
		if err != nil {
			t.Fatalf("VerifyStory(%s) error = %v", st.ID, err)
		}
		evID := articleEvent[string(checked.Articles[0].ID)]
		meta := eventMeta[evID]
		if !meta.Eligible && checked.VerificationPassed {
			unverifiedMarkedVerified++
			t.Errorf("ineligible event %s (%s) was marked VerificationPassed=true", evID, checked.Title)
		}
		if checked.VerificationPassed {
			scored, _, err := ranker.ScoreStory(ctx, checked, windowEnd)
			if err != nil {
				t.Fatalf("ScoreStory(%s) error = %v", checked.ID, err)
			}
			verified = append(verified, scored)
			verifiedByID[string(scored.ID)] = scored
		}
	}

	if unverifiedMarkedVerified != 0 {
		t.Fatalf("unverified-as-VERIFIED count = %d, want 0", unverifiedMarkedVerified)
	}

	if totalLLMResponses == 0 {
		t.Fatal("expected LLM responses during evaluation")
	}
	schemaCompliance := float64(schemaValidResponses) / float64(totalLLMResponses)
	if schemaCompliance < 0.95 {
		t.Fatalf("JSON schema compliance = %.2f%%, want >= 95%%", schemaCompliance*100)
	}

	selRes := selector.Select(verified, map[string]bool{})
	if len(selRes.Selected) != 3 {
		t.Fatalf("len(Selected) = %d, want 3", len(selRes.Selected))
	}
	for _, item := range selRes.Selected {
		st := verifiedByID[string(item.StoryID)]
		evID := articleEvent[string(st.Articles[0].ID)]
		if !eventMeta[evID].Eligible {
			t.Fatalf("ineligible event %s leaked into TOP-3", evID)
		}
		for _, a := range st.Articles {
			if articleInfo[string(a.ID)].IsOld {
				t.Fatalf("old article %s leaked into TOP-3", a.ID)
			}
		}
	}

	t.Logf("Eval metrics: stories=%d pairs=%d precision=%.1f%% false_merges=%d schema_compliance=%.1f%% top3=%d",
		len(clusterRes.Stories), totalPairs, precision*100, falseMerges, schemaCompliance*100, len(selRes.Selected))
}
