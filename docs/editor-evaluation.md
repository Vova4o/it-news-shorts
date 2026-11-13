# Editor Evaluation Dataset & Quality Gates

`testdata/editor_eval_dataset.json` contains 51 synthetic articles across 18 distinct IT events covering:
- Multi-source corroborated events
- Same-company different-product-version traps (e.g., `2.0` vs `3.0`)
- Security vulnerability vs product launch traps
- Rumors, sponsored/PR fluff, and contradicted numbers
- Out-of-window (>24h old) articles
- Prompt injection attempts embedded inside article text

`TestQualityEvaluationDataset` (`internal/editor/eval_test.go`) verifies:
- Clustering precision >= 90% and 0 false merges
- 0 unverified/contradicted/injected stories marked `VERIFIED`
- JSON schema compliance >= 95%
- 100% exclusion of old, duplicate, and unverified stories from TOP-3
