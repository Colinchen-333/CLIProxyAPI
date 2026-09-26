package executor

import (
	"net/http"
	"testing"
)

func TestUpstreamErrorStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"bare model echo seen from opencode go", 400, `{"model":"deepseek-v4.1-flash"}`, http.StatusBadGateway},
		{"empty body", 400, ``, http.StatusBadGateway},
		{"whitespace body", 400, " \n", http.StatusBadGateway},
		{"openai error object", 400, `{"error":{"message":"context length exceeded","type":"invalid_request_error"}}`, 400},
		{"opencode typed error", 400, `{"type":"error","error":{"type":"MissingSessionID","message":"missing"}}`, 400},
		{"top-level message", 400, `{"message":"bad tool schema"}`, 400},
		{"plain text reason", 400, `Bad Request: messages required`, 400},
		{"non-400 untouched", 429, `{"model":"x"}`, 429},
		{"500 untouched", 500, ``, 500},
	}
	for _, tc := range cases {
		if got := upstreamErrorStatus(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: upstreamErrorStatus(%d, %q) = %d, want %d", tc.name, tc.status, tc.body, got, tc.want)
		}
	}
}
