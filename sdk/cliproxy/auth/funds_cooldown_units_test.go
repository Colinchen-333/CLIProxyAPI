package auth

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestIsInsufficientFundsStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"zen-402", 402, "Upstream request failed: Insufficient account funds", true},
		{"credit-balance-402", 402, "Your credit balance is too low", true},
		{"billing-403", 403, "billing account disabled", true},
		{"payment-required-402", 402, "Payment Required", true},
		{"plain-402", 402, "upgrade your plan", false},
		{"empty-402", 402, "", false},
		{"quota-429", 429, "insufficient funds", false},
		{"transient-502", 502, "billing service unavailable", false},
		{"plain-403", 403, "forbidden by policy", false},
	}
	for _, tc := range cases {
		if got := isInsufficientFundsStatus(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: isInsufficientFundsStatus(%d, %q) = %v, want %v", tc.name, tc.status, tc.body, got, tc.want)
		}
	}
}

func TestPreferRetryableError(t *testing.T) {
	e402 := &fundsTestStatusErr{code: 402, msg: "funds"}
	e401 := &fundsTestStatusErr{code: 401, msg: "unauthorized"}
	e502 := &fundsTestStatusErr{code: 502, msg: "bad gateway"}
	e529 := &fundsTestStatusErr{code: 529, msg: "overloaded"}
	transport := errors.New("dial tcp: i/o timeout")
	e400 := &fundsTestStatusErr{code: 400, msg: "bad request"}

	if got := preferRetryableError(e502, e402); got != e502 {
		t.Errorf("502 then 402 must keep 502, got %v", got)
	}
	if got := preferRetryableError(e402, e502); got != e502 {
		t.Errorf("402 then 502 must return 502, got %v", got)
	}
	if got := preferRetryableError(transport, e402); got != transport {
		t.Errorf("transport error must beat 402, got %v", got)
	}
	if got := preferRetryableError(e401, e402); got != e402 {
		t.Errorf("equal ranks keep the latest, got %v", got)
	}
	if got := preferRetryableError(e502, e529); got != e529 {
		t.Errorf("equal ranks keep the latest, got %v", got)
	}
	if got := preferRetryableError(e400, e402); got != e400 {
		t.Errorf("generic 4xx must beat a credential-scoped 402, got %v", got)
	}
	if got := preferRetryableError(nil, e402); got != e402 {
		t.Errorf("nil prev returns next, got %v", got)
	}
	if got := preferRetryableError(e402, nil); got != e402 {
		t.Errorf("nil next keeps prev, got %v", got)
	}
}

func TestFundsCooldown_ClearedByReloadAndBySuccess(t *testing.T) {
	fundsErr := &fundsTestStatusErr{code: http.StatusPaymentRequired, msg: fundsTestBody}
	exec := newFundsTestExecutor(map[string]error{fundsAuthID(t, "a-unfunded"): fundsErr})
	m := newFundsTestManager(t, exec, []string{"a-unfunded", "b-healthy"}, "model-one")
	aID := fundsAuthID(t, "a-unfunded")

	for i := 0; i < 4 && exec.Calls(aID) == 0; i++ {
		if _, err := fundsExecute(m, "model-one"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if exec.Calls(aID) != 1 {
		t.Fatalf("a-unfunded should have been probed once, got %d", exec.Calls(aID))
	}
	if !fundsBlockedAt(t, m, aID, "model-one", time.Hour) {
		t.Fatal("a-unfunded must be blocked before the reload")
	}

	// The operator topped up the account and reloaded the config.
	exec.mu.Lock()
	delete(exec.failure, aID)
	exec.mu.Unlock()
	if cleared := m.ClearFundsCooldowns(); cleared != 1 {
		t.Fatalf("ClearFundsCooldowns cleared %d credentials, want 1", cleared)
	}
	if fundsBlockedAt(t, m, aID, "model-one", time.Hour) {
		t.Fatal("a-unfunded must be usable after the reload")
	}
	served := map[string]bool{}
	for i := 0; i < 6; i++ {
		got, err := fundsExecute(m, "model-one")
		if err != nil {
			t.Fatalf("post-reload request %d: %v", i, err)
		}
		served[got] = true
	}
	if !served[aID] {
		t.Fatalf("a-unfunded never served after the reload: %v", served)
	}
	if auth, _ := m.GetByID(aID); auth == nil || !auth.FundsExhaustedUntil.IsZero() {
		t.Fatal("funds block must be cleared after a successful request")
	}
}

func TestFundsCooldown_LongCooldownLength(t *testing.T) {
	auth := &Auth{ID: "x", Provider: "p", Status: StatusActive}
	now := time.Now()
	if !markFundsExhaustedLocked(auth, now) {
		t.Fatal("first mark must report a new block")
	}
	if got := auth.FundsExhaustedUntil.Sub(now); got != 24*time.Hour {
		t.Fatalf("cooldown %v, want 24h", got)
	}
	if markFundsExhaustedLocked(auth, now.Add(time.Minute)) {
		t.Fatal("second mark within the window must not report a new block or extend it")
	}
	if blocked, _, _ := isAuthBlockedForModel(auth, "any-model", now.Add(time.Hour)); !blocked {
		t.Fatal("funds block must apply to every model")
	}
}
