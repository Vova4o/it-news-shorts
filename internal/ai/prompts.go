package ai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/it-news-shorts/it-news-shorts/configs/prompts"
)

// PromptDefinition holds a versioned prompt template and its JSON Schema.
type PromptDefinition struct {
	Version            string            `json:"version"`
	Operation          string            `json:"operation"`
	SystemInstructions string            `json:"system_instructions"`
	Constraints        []string          `json:"constraints"`
	OutputSchema       json.RawMessage   `json:"output_schema"`
	ValidExamples      []json.RawMessage `json:"valid_examples"`
	RejectedExamples   []json.RawMessage `json:"rejected_examples"`
}

// PromptRegistry stores versioned prompts indexed by operation.
type PromptRegistry struct {
	byOp map[string]PromptDefinition
}

// LoadEmbeddedPrompts loads all embedded v1 editorial prompts.
func LoadEmbeddedPrompts() (*PromptRegistry, error) {
	paths := []string{
		"clustering/v1.json",
		"fact-extraction/v1.json",
		"fact-verification/v1.json",
		"scoring/v1.json",
	}
	reg := &PromptRegistry{
		byOp: make(map[string]PromptDefinition, len(paths)),
	}
	for _, p := range paths {
		raw, err := prompts.FS.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("ai: read prompt %s: %w", p, err)
		}
		var def PromptDefinition
		if err := json.Unmarshal(raw, &def); err != nil {
			return nil, fmt.Errorf("ai: decode prompt %s: %w", p, err)
		}
		if strings.TrimSpace(def.Version) == "" || strings.TrimSpace(def.Operation) == "" {
			return nil, fmt.Errorf("ai: prompt %s missing version or operation", p)
		}
		if len(def.OutputSchema) == 0 {
			return nil, fmt.Errorf("ai: prompt %s missing output_schema", p)
		}
		reg.byOp[def.Operation] = def
	}
	return reg, nil
}

// Get returns the PromptDefinition for an operation.
func (r *PromptRegistry) Get(operation string) (PromptDefinition, error) {
	if r == nil {
		return PromptDefinition{}, fmt.Errorf("ai: prompt registry is nil")
	}
	def, ok := r.byOp[operation]
	if !ok {
		return PromptDefinition{}, fmt.Errorf("ai: prompt for operation %q not found", operation)
	}
	return def, nil
}
