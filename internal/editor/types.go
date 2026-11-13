package editor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
)

// StoryCategory classifies an IT news story for diversity and reporting.
type StoryCategory string

const (
	CategoryAI            StoryCategory = "AI"
	CategorySoftwareDev   StoryCategory = "SOFTWARE_DEV"
	CategoryCybersecurity StoryCategory = "CYBERSECURITY"
	CategoryHardware      StoryCategory = "HARDWARE"
	CategoryCloud         StoryCategory = "CLOUD"
	CategoryBigTech       StoryCategory = "BIG_TECH"
	CategoryRobotics      StoryCategory = "ROBOTICS"
	CategoryConsumerTech  StoryCategory = "CONSUMER_TECH"
	CategoryOtherIT       StoryCategory = "OTHER_IT"
)

// Valid reports whether the category is known.
func (c StoryCategory) Valid() bool {
	switch c {
	case CategoryAI, CategorySoftwareDev, CategoryCybersecurity, CategoryHardware,
		CategoryCloud, CategoryBigTech, CategoryRobotics, CategoryConsumerTech, CategoryOtherIT:
		return true
	default:
		return false
	}
}

// ClaimType classifies an extracted factual claim.
type ClaimType string

const (
	ClaimEvent        ClaimType = "EVENT"
	ClaimProduct      ClaimType = "PRODUCT"
	ClaimDate         ClaimType = "DATE"
	ClaimNumber       ClaimType = "NUMBER"
	ClaimTechnical    ClaimType = "TECHNICAL"
	ClaimQuote        ClaimType = "QUOTE"
	ClaimAvailability ClaimType = "AVAILABILITY"
	ClaimComparison   ClaimType = "COMPARISON"
)

// VerificationStatus represents the fact-checking verdict for a claim.
type VerificationStatus string

const (
	StatusVerified             VerificationStatus = "VERIFIED"
	StatusPartiallyVerified    VerificationStatus = "PARTIALLY_VERIFIED"
	StatusUnverified           VerificationStatus = "UNVERIFIED"
	StatusContradicted         VerificationStatus = "CONTRADICTED"
	StatusInsufficientEvidence VerificationStatus = "INSUFFICIENT_EVIDENCE"
)

// StoryArticleLink ties an article to a clustered story.
type StoryArticleLink struct {
	ArticleID  string
	Role       string // PRIMARY, CORROBORATING, UPDATE
	Confidence float64
	Reason     string
}

// ClusteredStory is a deduplicated real-world IT event backed by one or more
// collected articles.
type ClusteredStory struct {
	ID                  domain.StoryID
	Title               string
	Summary             string
	Category            StoryCategory
	Fingerprint         string
	Confidence          float64
	ClusterReason       string
	IsMultiDayUpdate    bool
	IsSignificantUpdate bool
	Articles            []news.UnifiedArticle
	ArticleLinks        []StoryArticleLink
	Claims              []Claim
	VerificationPassed  bool
	RejectionReason     string
	Score               *StoryScore
	CreatedAt           time.Time
}

// Claim is a single atomic factual statement extracted from a story.
type Claim struct {
	ID                 string
	StoryID            domain.StoryID
	Text               string
	Type               ClaimType
	SourceArticleIDs   []string
	Entities           []string
	Numbers            []string
	EvidenceQuote      string
	Confidence         float64
	VerificationStatus VerificationStatus
	RejectionReason    string
	Evidence           []ClaimEvidence
}

// ClaimEvidence links a claim to a concrete source URL and excerpt.
type ClaimEvidence struct {
	ID                 string
	ClaimID            string
	ArticleID          string
	SourceURL          string
	SourceName         string
	Snippet            string
	IsPrimary          bool
	IsIndependent      bool
	AccessedAt         time.Time
	AvailabilityStatus string // AVAILABLE, UNAVAILABLE, CACHED
}

// StoryScore stores the 4 sub-scores and deterministic weighted final score.
type StoryScore struct {
	StoryID          domain.StoryID
	Impact           float64
	Interest         float64
	Freshness        float64
	Novelty          float64
	FinalScore       float64
	ReasoningSummary string
	ModelVersion     string
	PromptVersion    string
	CalculatedAt     time.Time
}

// SelectedStoryItem is one entry in the TOP-3 editorial selection.
type SelectedStoryItem struct {
	Rank             int      `json:"rank"`
	StoryID          string   `json:"story_id"`
	Title            string   `json:"title"`
	Category         string   `json:"category"`
	Score            float64  `json:"score"`
	VerifiedClaimIDs []string `json:"verified_claim_ids"`
	SourceURLs       []string `json:"source_urls"`
	SelectionReason  string   `json:"selection_reason"`
	Fingerprint      string   `json:"fingerprint,omitempty"`
}

// EditorialReport is the structured output of DailyEditorialWorkflow.
type EditorialReport struct {
	Date              string              `json:"date"`
	EditorialRunID    string              `json:"editorial_run_id"`
	SnapshotID        string              `json:"snapshot_id"`
	WorkflowID        string              `json:"workflow_id"`
	Status            string              `json:"status"`
	ArticlesProcessed int                 `json:"articles_processed"`
	UniqueStories     int                 `json:"unique_stories"`
	VerifiedStories   int                 `json:"verified_stories"`
	RejectedStories   int                 `json:"rejected_stories"`
	Selected          []SelectedStoryItem `json:"selected"`
	LLMRequests       int                 `json:"llm_requests"`
	InputTokens       int                 `json:"input_tokens"`
	OutputTokens      int                 `json:"output_tokens"`
	TotalCostUSD      float64             `json:"total_cost_usd"`
	DurationMS        int64               `json:"duration_ms"`
}

// LLMUsageRecord captures telemetry and cost for one LLM request.
type LLMUsageRecord struct {
	EditorialRunID string
	StoryID        string
	Operation      string
	Provider       string
	Model          string
	PromptVersion  string
	RequestID      string
	InputTokens    int
	OutputTokens   int
	CostUSD        float64
	DurationMS     int64
	Status         string
	ErrorMessage   string
}

// DeterministicStoryID builds a stable StoryID from a canonical fingerprint.
func DeterministicStoryID(fingerprint string) domain.StoryID {
	sum := sha256.Sum256([]byte(strings.TrimSpace(fingerprint)))
	return domain.StoryID("story-" + hex.EncodeToString(sum[:12]))
}

// StoryFingerprint computes a stable normalized event key from title and
// primary entities.
func StoryFingerprint(title string) string {
	norm := news.NormalizeTitleForDedup(title)
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:16])
}

var injectionLineRe = regexp.MustCompile(`(?im)^.*(ignore previous instructions|system prompt|you are now|disregard all|override rules|mark as verified|score\s*=\s*100).*$`)

// SanitizeUntrustedArticleText strips common prompt-injection lines and wraps
// untrusted news text so LLM prompts clearly isolate data from instructions.
func SanitizeUntrustedArticleText(raw string) string {
	cleaned := injectionLineRe.ReplaceAllString(raw, "[REDACTED_INJECTION_ATTEMPT]")
	return strings.TrimSpace(cleaned)
}

// ContainsPromptInjection reports whether raw contains suspicious instruction
// override patterns.
func ContainsPromptInjection(raw string) bool {
	return injectionLineRe.MatchString(raw)
}

// FormatArticleBlock formats an article safely as untrusted data for LLM input.
func FormatArticleBlock(a news.UnifiedArticle) string {
	pub := "unknown"
	if a.PublishedAt != nil {
		pub = a.PublishedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"article_id: %s\nsource_id: %s\nurl: %s\npublished_at: %s\ntitle: %s\ndescription: %s",
		a.ID,
		a.SourceID,
		a.CanonicalURL,
		pub,
		SanitizeUntrustedArticleText(a.Title),
		SanitizeUntrustedArticleText(a.Description),
	)
}
