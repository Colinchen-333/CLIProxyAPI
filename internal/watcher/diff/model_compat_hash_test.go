package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestModelHashesIncludeIsCompat(t *testing.T) {
	if ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "m"}}) == ComputeOpenAICompatModelsHash([]config.OpenAICompatibilityModel{{Name: "m", IsCompat: true}}) {
		t.Fatal("OpenAI compatibility model hash did not change when IsCompat changed")
	}
}
