package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAPIKeyModelIsCompatConfigDecoding(t *testing.T) {
	const yamlConfig = `openai-compatibility:
  - name: deepseek
    models:
      - name: deepseek-upstream
        alias: deepseek-alias
        is-compat: true
      - name: openai-native
        alias: openai-native
`

	var cfg Config
	if errDecode := yaml.Unmarshal([]byte(yamlConfig), &cfg); errDecode != nil {
		t.Fatalf("decode error: %v", errDecode)
	}

	if len(cfg.OpenAICompatibility) != 1 || !cfg.OpenAICompatibility[0].Models[0].IsCompat {
		t.Fatalf("openai-compatibility IsCompat = %+v, want true", cfg.OpenAICompatibility)
	}
	if cfg.OpenAICompatibility[0].Models[1].IsCompat {
		t.Fatal("openai-compatibility omitted IsCompat = true, want default false")
	}
	if !cfg.OpenAICompatibility[0].Models[0].GetIsCompat() {
		t.Fatal("OpenAICompatibilityModel GetIsCompat() = false, want true")
	}
}
