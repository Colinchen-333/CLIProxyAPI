package helps

import (
	"net/http"
	"strings"
)

// ApplyOpenCodeSessionHeader follows OpenCode Go's routing/cache contract while
// retaining explicit operator headers. Other upstream hosts are unchanged.
func ApplyOpenCodeSessionHeader(r *http.Request) {
	if r == nil || r.URL == nil || !strings.EqualFold(r.URL.Hostname(), "opencode.ai") || !strings.HasPrefix(r.URL.Path, "/zen/go/") {
		return
	}
	if r.Header.Get("X-Opencode-Session") != "" {
		return
	}
	session := strings.TrimSpace(r.Header.Get("Session_id"))
	if session != "" {
		r.Header.Set("X-Opencode-Session", session)
	}
}
