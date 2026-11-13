# Story Clustering

`internal/editor/clusterer.go` (`StoryClusterer`) groups collected `news.UnifiedArticle` items into `ClusteredStory` events.

## Rules & Safeguards

- **Prompt-Injection Defense**: Every article block is sanitized via `SanitizeUntrustedArticleText` before being sent to the LLM.
- **Anti-False-Merge Rules**:
  - Distinct product versions (e.g., `2.0` vs `3.0`) are kept separate.
  - Security vulnerabilities / CVEs are never merged with product launch announcements.
  - Unconfirmed rumors are never merged with confirmed official releases.
- **Deterministic IDs**: Each cluster receives a normalized `StoryFingerprint` (`StoryFingerprint(canonicalTitle)`) and deterministic `StoryID` (`DeterministicStoryID(fingerprint)`).
- **Traceability**: Every article link in `story_articles` records its `Role` (`PRIMARY`, `CORROBORATING`, `UPDATE`), `Confidence`, and `Reason`.
