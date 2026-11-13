# News Ranking & TOP-3 Selection

## Scoring Formula

`internal/editor/ranker.go` (`StoryRankerEngine`, `CalculateFinalScore`) computes the final 0–100 score deterministically in Go:

```text
FinalScore = Impact * 0.35 + Interest * 0.25 + Freshness * 0.20 + Novelty * 0.20
```

- **Impact (35%)**: Significance for the IT industry, developers, infrastructure, or security.
- **Interest (25%)**: Broad audience appeal for 15–30s YouTube Shorts.
- **Freshness (20%)**: Original publication recency (not Hacker News submission time).
- **Novelty (20%)**: New development vs routine follow-up/reprint.

Verification is a **mandatory prerequisite**, not bonus points.

## TOP-3 Selection (`DailyStorySelector`)

`internal/editor/top_selector.go`:
- Filters out any story with `VerificationPassed == false`.
- Filters out stories whose `Fingerprint` appeared in `daily_selection_items` over the last 7 days.
- Sorts eligible candidates by `FinalScore` descending.
- Enforces category diversity: at most 2 stories from the same `StoryCategory` when eligible alternatives exist.
- Never fabricates placeholder stories if fewer than 3 verified stories exist.
