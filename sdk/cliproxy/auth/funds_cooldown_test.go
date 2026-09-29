package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const (
	fundsTestProvider = "fundstest"
	fundsTestBody     = "Upstream request failed: Insufficient account funds"
)

type fundsTestStatusErr struct {
	code int
	msg  string
}

func (e *fundsTestStatusErr) Error() string   { return e.msg }
func (e *fundsTestStatusErr) StatusCode() int { return e.code }

// fundsTestExecutor answers per auth: a nil script entry means success.
type fundsTestExecutor struct {
	mu      sync.Mutex
	failure map[string]error
	calls   map[string]int
}

func newFundsTestExecutor(failure map[string]error) *fundsTestExecutor {
	return &fundsTestExecutor{failure: failure, calls: make(map[string]int)}
}

func (e *fundsTestExecutor) Identifier() string { return fundsTestProvider }

func (e *fundsTestExecutor) record(authID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls[authID]++
	return e.failure[authID]
}

func (e *fundsTestExecutor) Calls(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[authID]
}

func (e *fundsTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record(auth.ID); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *fundsTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := e.record(auth.ID); err != nil {
		return nil, err
	}
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (e *fundsTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }

func (e *fundsTestExecutor) CountTokens(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := e.record(auth.ID); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *fundsTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, &Error{HTTPStatus: http.StatusNotImplemented, Message: "HttpRequest not implemented"}
}

// newFundsTestManager registers one auth per id, all serving the given models.
func newFundsTestManager(t *testing.T, exec *fundsTestExecutor, authIDs []string, models ...string) *Manager {
	t.Helper()
	// Production runs with transient-error-cooldown-seconds: -1 (transient errors never
	// cool a key down), which is what makes the retryable error reach the client.
	SetTransientErrorCooldownSeconds(-1)
	t.Cleanup(func() { SetTransientErrorCooldownSeconds(0) })
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	m.RegisterExecutor(exec)
	reg := registry.GetGlobalRegistry()
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	for _, id := range authIDs {
		authID := t.Name() + "/" + id
		if _, err := m.Register(context.Background(), &Auth{ID: authID, Provider: fundsTestProvider, Status: StatusActive}); err != nil {
			t.Fatalf("register auth %s: %v", id, err)
		}
		reg.RegisterClient(authID, fundsTestProvider, infos)
		t.Cleanup(func() { reg.UnregisterClient(authID) })
	}
	return m
}

func fundsAuthID(t *testing.T, id string) string { return t.Name() + "/" + id }

func fundsExecute(m *Manager, model string) (string, error) {
	resp, err := m.Execute(context.Background(), []string{fundsTestProvider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	return string(resp.Payload), err
}

func fundsExecuteStream(m *Manager, model string) (string, error) {
	res, err := m.ExecuteStream(context.Background(), []string{fundsTestProvider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != nil {
		return "", err
	}
	var out []byte
	for chunk := range res.Chunks {
		if chunk.Err != nil {
			return "", chunk.Err
		}
		out = append(out, chunk.Payload...)
	}
	return string(out), nil
}

func fundsBlockedAt(t *testing.T, m *Manager, authID, model string, offset time.Duration) bool {
	t.Helper()
	auth, ok := m.GetByID(authID)
	if !ok {
		t.Fatalf("auth %s not found", authID)
	}
	blocked, _, _ := isAuthBlockedForModel(auth, model, time.Now().Add(offset))
	return blocked
}

type fundsRequestFn func(m *Manager, model string) (string, error)

var fundsRequestModes = map[string]fundsRequestFn{
	"execute": fundsExecute,
	"stream":  fundsExecuteStream,
}

// Owner scenario: three keys behind one pool, two are out of funds and one works.
// The request must reach the working key, and later requests (for any model the
// pool serves) must not probe the unfunded keys again.
func TestFundsCooldown_TwoUnfundedOneHealthy(t *testing.T) {
	for mode, run := range fundsRequestModes {
		t.Run(mode, func(t *testing.T) {
			ids := []string{"a-unfunded", "b-unfunded", "c-healthy"}
			fundsErr := &fundsTestStatusErr{code: http.StatusPaymentRequired, msg: fundsTestBody}
			exec := newFundsTestExecutor(map[string]error{
				fundsAuthID(t, "a-unfunded"): fundsErr,
				fundsAuthID(t, "b-unfunded"): fundsErr,
			})
			m := newFundsTestManager(t, exec, ids, "model-one", "model-two")

			// Drive requests until the round-robin walk has probed both unfunded keys once.
			for i := 0; i < 6 && (exec.Calls(fundsAuthID(t, "a-unfunded")) == 0 || exec.Calls(fundsAuthID(t, "b-unfunded")) == 0); i++ {
				got, err := run(m, "model-one")
				if err != nil {
					t.Fatalf("request must succeed on the healthy key, got %v", err)
				}
				if got != fundsAuthID(t, "c-healthy") {
					t.Fatalf("served by %q, want the healthy key", got)
				}
			}
			firstA, firstB := exec.Calls(fundsAuthID(t, "a-unfunded")), exec.Calls(fundsAuthID(t, "b-unfunded"))
			if firstA != 1 || firstB != 1 {
				t.Fatalf("each unfunded key should be probed exactly once so far, got a=%d b=%d", firstA, firstB)
			}

			// Later requests: same model and the pool's other model.
			for i, model := range []string{"model-one", "model-two", "model-one", "model-two"} {
				got, err := run(m, model)
				if err != nil {
					t.Fatalf("later request %d (%s) failed: %v", i+1, model, err)
				}
				if got != fundsAuthID(t, "c-healthy") {
					t.Fatalf("later request %d (%s) served by %q, want the healthy key", i+1, model, got)
				}
			}
			if a, b := exec.Calls(fundsAuthID(t, "a-unfunded")), exec.Calls(fundsAuthID(t, "b-unfunded")); a != firstA || b != firstB {
				t.Fatalf("unfunded keys were probed again: a=%d (was %d) b=%d (was %d)", a, firstA, b, firstB)
			}

			// The block is long, not the 30 minute payment_required window.
			for _, id := range []string{"a-unfunded", "b-unfunded"} {
				for _, model := range []string{"model-one", "model-two"} {
					if !fundsBlockedAt(t, m, fundsAuthID(t, id), model, 31*time.Minute) {
						t.Fatalf("%s/%s must still be blocked after 31 minutes", id, model)
					}
					if !fundsBlockedAt(t, m, fundsAuthID(t, id), model, 23*time.Hour) {
						t.Fatalf("%s/%s must still be blocked after 23 hours", id, model)
					}
					if fundsBlockedAt(t, m, fundsAuthID(t, id), model, 25*time.Hour) {
						t.Fatalf("%s/%s must be probed again after 25 hours", id, model)
					}
				}
			}
			if fundsBlockedAt(t, m, fundsAuthID(t, "c-healthy"), "model-one", 0) {
				t.Fatal("healthy key must never be blocked")
			}
		})
	}
}

// (i) Pool of two: A fails transiently, B answers 402 funds. The client must get
// the retryable error, not B's credential-scoped 402. (ii) B is then not picked
// again for well over 30 minutes.
func TestFundsCooldown_TransientPlusFundsReturnsRetryable(t *testing.T) {
	for mode, run := range fundsRequestModes {
		t.Run(mode, func(t *testing.T) {
			exec := newFundsTestExecutor(map[string]error{
				fundsAuthID(t, "a-transient"): &fundsTestStatusErr{code: http.StatusBadGateway, msg: "upstream reset"},
				fundsAuthID(t, "b-unfunded"):  &fundsTestStatusErr{code: http.StatusPaymentRequired, msg: fundsTestBody},
			})
			m := newFundsTestManager(t, exec, []string{"a-transient", "b-unfunded"}, "model-one")

			_, err := run(m, "model-one")
			if err == nil {
				t.Fatal("expected an error, both keys fail")
			}
			status := statusCodeFromError(err)
			if status == http.StatusPaymentRequired {
				t.Fatalf("client got the credential-scoped 402 although the other key only failed transiently: %v", err)
			}
			if status != http.StatusBadGateway {
				t.Fatalf("client should get the retryable upstream error (502), got status %d: %v", status, err)
			}
			if b := exec.Calls(fundsAuthID(t, "b-unfunded")); b != 1 {
				t.Fatalf("b-unfunded probed %d times in the first request, want 1", b)
			}

			for i := 0; i < 3; i++ {
				_, err = run(m, "model-one")
				if got := statusCodeFromError(err); got != http.StatusBadGateway {
					t.Fatalf("repeat request %d: status %d, want 502 from the transient key: %v", i, got, err)
				}
			}
			if b := exec.Calls(fundsAuthID(t, "b-unfunded")); b != 1 {
				t.Fatalf("b-unfunded probed again (%d calls in total)", b)
			}
			if !fundsBlockedAt(t, m, fundsAuthID(t, "b-unfunded"), "model-one", 31*time.Minute) {
				t.Fatal("b-unfunded must stay blocked beyond 30 minutes")
			}
			if fundsBlockedAt(t, m, fundsAuthID(t, "a-transient"), "model-one", 0) {
				t.Fatal("transient errors must not put the key into a funds block")
			}
		})
	}
}

// (iii) A 402 on one credential with a healthy second credential succeeds on the
// second one. Bodies that do not describe an empty balance keep the short cooldown.
func TestFundsCooldown_PaymentRequiredFailsOverToHealthyKey(t *testing.T) {
	bodies := map[string]struct {
		body     string
		longLive bool
	}{
		"insufficient-funds": {fundsTestBody, true},
		"plain-402":          {"upgrade your plan", false},
	}
	for name, tc := range bodies {
		for mode, run := range fundsRequestModes {
			t.Run(name+"/"+mode, func(t *testing.T) {
				exec := newFundsTestExecutor(map[string]error{
					fundsAuthID(t, "a-402"): &fundsTestStatusErr{code: http.StatusPaymentRequired, msg: tc.body},
				})
				m := newFundsTestManager(t, exec, []string{"a-402", "b-healthy"}, "model-one")

				got, err := run(m, "model-one")
				if err != nil {
					t.Fatalf("request must succeed on the healthy key, got %v", err)
				}
				if got != fundsAuthID(t, "b-healthy") {
					t.Fatalf("served by %q, want the healthy key", got)
				}
				blockedAt31 := fundsBlockedAt(t, m, fundsAuthID(t, "a-402"), "model-one", 31*time.Minute)
				if blockedAt31 != tc.longLive {
					t.Fatalf("blocked after 31 minutes = %v, want %v", blockedAt31, tc.longLive)
				}
				if !fundsBlockedAt(t, m, fundsAuthID(t, "a-402"), "model-one", 5*time.Minute) {
					t.Fatal("a 402 key must be skipped within the first minutes")
				}
			})
		}
	}
}

// When every credential is out of funds the honest answer is still the 402.
func TestFundsCooldown_AllUnfundedReturnsPaymentRequired(t *testing.T) {
	for mode, run := range fundsRequestModes {
		t.Run(mode, func(t *testing.T) {
			fundsErr := &fundsTestStatusErr{code: http.StatusPaymentRequired, msg: fundsTestBody}
			exec := newFundsTestExecutor(map[string]error{
				fundsAuthID(t, "a"): fundsErr,
				fundsAuthID(t, "b"): fundsErr,
			})
			m := newFundsTestManager(t, exec, []string{"a", "b"}, "model-one")
			_, err := run(m, "model-one")
			if got := statusCodeFromError(err); got != http.StatusPaymentRequired {
				t.Fatalf("status %d, want 402 when every credential is unfunded: %v", got, err)
			}
		})
	}
}
