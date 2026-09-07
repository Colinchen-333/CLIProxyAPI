package splitrelay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/requestmeta"
)

func TestOwnSessionPseudonymAndIsolation(t *testing.T) {
	parsed := map[string]json.RawMessage{"metadata": json.RawMessage(`{"user_id":"{\"session_id\":\"one\",\"device_id\":\"private-device\"}"}`)}
	first := ownSession(nil, parsed, "key")
	if len(first) != 64 || strings.Contains(first, "one") {
		t.Fatal("expected opaque session pseudonym")
	}
	old := map[string]json.RawMessage{"metadata": json.RawMessage(`{"user_id":"user_private_account_private_session_one"}`)}
	if first != ownSession(nil, old, "key") {
		t.Fatal("legacy and JSON IDs must converge")
	}
	if first == ownSession(nil, parsed, "different-key") {
		t.Fatal("gateway scopes must differ")
	}
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"two"}}
	if first == ownSession(headers, parsed, "key") {
		t.Fatal("different sessions must differ")
	}
	if ownSession(nil, nil, "key") != "" {
		t.Fatal("must not fabricate content-based identity")
	}
}

func TestOwnSessionOnlyInternalWithoutOriginalIdentity(t *testing.T) {
	h := makeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "metadata") || strings.Contains(string(body), "private") {
			t.Error("identity leaked into own body")
		}
		id := requestmeta.OwnSession(r.Context())
		if len(id) != 64 || r.Header.Get("X-Session-ID") != id {
			t.Error("missing trusted session and affinity identity")
		}
		if r.Header.Get("X-Claude-Code-Session-Id") != "" || r.Header.Get("Cookie") != "" {
			t.Error("official headers leaked")
		}
		w.WriteHeader(200)
	}), nil)
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"own","messages":[],"metadata":{"user_id":"{\"session_id\":\"private-session\"}"}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", "private-cookie")
	h.ServeHTTP(httptest.NewRecorder(), r)
}
