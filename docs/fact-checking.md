# Fact Checking & Anti-Hallucination

`internal/editor/extractor.go` (`FactExtractor`) and `internal/editor/factchecker.go` (`FactCheckerEngine`) enforce strict anti-hallucination guarantees:

1. **Source-Anchored Claims**: Claims referencing non-existent `article_id`s are immediately dropped by `FactExtractor`.
2. **Numeric Grounding**: Any number extracted in a claim must appear in the underlying article text; ungrounded numbers cannot be marked `VERIFIED`.
3. **Syndication / Reprint Detection**: Articles sharing identical body text across multiple sites do not count as independent confirmations.
4. **Contradiction Rejection**: Any claim with `StatusContradicted` immediately fails story verification (`VerificationPassed = false`).
5. **Minimum Evidence Gate**: A story requires at least `MinVerifiedClaims` verified claims (or an official primary source domain) to pass verification. Unverified or contradicted stories can never enter TOP-3 regardless of score.
