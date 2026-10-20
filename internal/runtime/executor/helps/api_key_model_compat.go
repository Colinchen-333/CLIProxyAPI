package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	openaiclaude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/claude"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// OpenAICompatModelIsCompat reports whether the openai-compatibility model bound
// to this attempt enables compatibility handling for Claude thinking blocks.
//
// The answer is computed from the config block that produced this credential
// (compat_name attribute) and the upstream model this attempt dispatches to.
// It is deliberately not cached per credential: one credential serves several
// upstream models, and only the flag on the matching model entry applies.
func OpenAICompatModelIsCompat(cfg *config.Config, auth *cliproxyauth.Auth, model string) bool {
	if cfg == nil || auth == nil || auth.Attributes == nil {
		return false
	}
	compatName := strings.TrimSpace(auth.Attributes["compat_name"])
	if compatName == "" {
		return false
	}
	for i := range cfg.OpenAICompatibility {
		block := &cfg.OpenAICompatibility[i]
		if block.Disabled || !strings.EqualFold(strings.TrimSpace(block.Name), compatName) {
			continue
		}
		return openAICompatModelFlag(block.Models, model)
	}
	return false
}

// openAICompatModelFlag matches a configured model entry to a dispatched model.
//
// The executor receives the upstream name whenever routing resolved an alias
// pool, so name matching decides first and alias matching is only a fallback.
// Keeping the two passes separate matters when several entries share an alias
// to form a pool: an alias match must never borrow the flag from a sibling
// upstream that happens to sit earlier in the list.
func openAICompatModelFlag(models []config.OpenAICompatibilityModel, model string) bool {
	requested := strings.TrimSpace(model)
	if requested == "" {
		return false
	}
	baseModel := strings.TrimSpace(thinking.ParseSuffix(requested).ModelName)
	if baseModel == "" {
		baseModel = requested
	}
	match := func(candidate string) bool {
		if candidate == "" {
			return false
		}
		return strings.EqualFold(candidate, requested) || strings.EqualFold(candidate, baseModel)
	}
	for i := range models {
		if match(strings.TrimSpace(models[i].Name)) {
			return models[i].IsCompat
		}
	}
	for i := range models {
		entry := &models[i]
		name := strings.TrimSpace(entry.Name)
		if name != "" && match(name) {
			// Already answered by the first pass.
			continue
		}
		if match(strings.TrimSpace(entry.Alias)) {
			return entry.IsCompat
		}
	}
	return false
}

// TranslateRequestWithAPIKeyModelCompatibility applies compatibility-aware
// request translators when a configured API-key model enables compatibility mode.
func TranslateRequestWithAPIKeyModelCompatibility(cfg *config.Config, auth *cliproxyauth.Auth, model string, from, to sdktranslator.Format, baseModel string, payload []byte, stream bool) []byte {
	if payload == nil {
		return nil
	}
	if from == sdktranslator.FormatClaude && to == sdktranslator.FormatOpenAI &&
		OpenAICompatModelIsCompat(cfg, auth, model) {
		return openaiclaude.ConvertClaudeRequestToOpenAIWithCompat(baseModel, payload, stream)
	}
	return sdktranslator.TranslateRequest(from, to, baseModel, payload, stream)
}
