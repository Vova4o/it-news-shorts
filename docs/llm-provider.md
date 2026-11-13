# LLM Provider & Prompt Governance

## Provider (`internal/ai/`)

- `LLMProvider` (`internal/ai/llm.go`) abstracts structured JSON-schema generation.
- `OpenAIProvider` (`internal/ai/openai.go`) supports any OpenAI-compatible Chat Completions endpoint (`LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODEL`), enforcing:
  - Per-request timeout (`LLM_TIMEOUT`)
  - Concurrency semaphore (`LLM_MAX_CONCURRENT_REQUESTS`)
  - Retries on 429/5xx/timeouts with backoff (no retry on auth/budget/token errors)
  - Daily USD spend ceiling (`BudgetTracker` + `LLM_DAILY_BUDGET_USD`)
  - JSON Schema validation (`ValidateAgainstSchema`) against embedded schemas
- `MockLLMProvider` (`internal/ai/mock.go`) enables deterministic offline unit, evaluation, and integration testing.

## Versioned Prompts (`configs/prompts/`)

Embedded via `configs/prompts/embed.go`:
- `clustering/v1.json`
- `fact-extraction/v1.json`
- `fact-verification/v1.json`
- `scoring/v1.json`
