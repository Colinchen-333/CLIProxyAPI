package helps

import (
	"net/http/httptest"
	"testing"
)

func TestOpenCodeSessionScopedAndStable(t *testing.T) {
	for _, u := range []string{"https://opencode.ai/zen/go/v1/responses", "https://api.anthropic.com/v1/messages", "https://opencode.ai.evil.test/zen/go/v1/responses", "https://opencode.ai/zen/v1/responses"} {
		r := httptest.NewRequest("POST", u, nil)
		r.Header.Set("Session_id", "anonymous-stable-session")
		ApplyOpenCodeSessionHeader(r)
		expected := ""
		if u == "https://opencode.ai/zen/go/v1/responses" {
			expected = "anonymous-stable-session"
		}
		if r.Header.Get("X-Opencode-Session") != expected {
			t.Fatalf("unexpected routing header for %s", u)
		}
	}
	r := httptest.NewRequest("POST", "https://opencode.ai/zen/go/v1/responses", nil)
	ApplyOpenCodeSessionHeader(r)
	if r.Header.Get("X-Opencode-Session") != "" {
		t.Fatal("must not fabricate random session")
	}
	r.Header.Set("X-Opencode-Session", "operator-session")
	r.Header.Set("Session_id", "native-session")
	ApplyOpenCodeSessionHeader(r)
	if r.Header.Get("X-Opencode-Session") != "operator-session" {
		t.Fatal("operator override lost")
	}
}
