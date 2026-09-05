package agent

import (
	"errors"

	"github.com/guygrigsby/llm"
	"github.com/guygrigsby/llm/openrouter"
)

// Models builds one llm.LLM per OpenRouter model id. One key for every tier.
type Models struct {
	Key   string
	Title string
}

// Tier returns the adapter for modelID.
func (m Models) Tier(modelID string) (llm.LLM, error) {
	if m.Key == "" {
		return nil, errors.New("agent: OpenRouter key is empty")
	}
	if modelID == "" {
		return nil, errors.New("agent: model id is empty")
	}
	return openrouter.New(openrouter.Config{APIKey: m.Key, Model: modelID, Title: m.Title, Referer: "https://github.com/guygrigsby/autophage"})
}
