package splitrelay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// ownSession keeps only a keyed pseudonym, never the official account or device.
// No content-derived fallback: unrelated sessions must not share a cache identity.
// A Claude Code subagent is its own conversation (X-Claude-Code-Agent-Id), so it
// gets its own identity, as upstream keys it "claude:<session>:agent:<agent>";
// otherwise every subagent of one session is pinned to the same credential.
func ownSession(headers http.Header, parsed map[string]json.RawMessage, gatewayKey string) string {
	session := strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id"))
	if session == "" {
		var metadata struct {
			UserID string `json:"user_id"`
		}
		_ = json.Unmarshal(parsed["metadata"], &metadata)
		var identity struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(metadata.UserID), &identity) == nil {
			session = strings.TrimSpace(identity.SessionID)
		} else if _, suffix, ok := strings.Cut(metadata.UserID, "_session_"); ok {
			session = strings.TrimSpace(suffix)
		}
	}
	if session == "" || len(session) > 4096 {
		return ""
	}
	agent := strings.TrimSpace(headers.Get("X-Claude-Code-Agent-Id"))
	if agent == "main" || len(agent) > 4096 {
		agent = ""
	}
	mac := hmac.New(sha256.New, []byte(gatewayKey))
	_, _ = mac.Write([]byte("cliproxy:own-session:v1\x00"))
	_, _ = mac.Write([]byte(session))
	if agent != "" {
		_, _ = mac.Write([]byte("\x00agent\x00"))
		_, _ = mac.Write([]byte(agent))
	}
	return hex.EncodeToString(mac.Sum(nil))
}
