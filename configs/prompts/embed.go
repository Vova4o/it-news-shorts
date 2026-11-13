package prompts

import "embed"

// FS embeds all versioned editorial prompt definitions.
//
//go:embed clustering/*.json fact-extraction/*.json fact-verification/*.json scoring/*.json
var FS embed.FS
