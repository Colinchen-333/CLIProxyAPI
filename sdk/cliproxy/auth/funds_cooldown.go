package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// fundsExhaustedCooldown is how long a credential stays out of rotation after the
// upstream reports its account is out of funds. Topping up an account is a manual
// act, so probing every few minutes only burns requests; a config reload or a
// restart clears the block immediately.
const fundsExhaustedCooldown = 24 * time.Hour

const fundsExhaustedStatusMessage = "insufficient_funds"

// fundsErrorMarkers are lower-case fragments of upstream bodies that mean the
// account behind the credential has no balance left.
var fundsErrorMarkers = [...]string{
	"insufficient account funds",
	"insufficient funds",
	"insufficient fund",
	"insufficient balance",
	"insufficient credit",
	"insufficient_balance",
	"insufficient_credit",
	"insufficient_funds",
	"out of credit",
	"out of funds",
	"no credits",
	"credit balance",
	"account balance",
	"billing",
	"payment required",
	"payment_required",
	"top up",
	"recharge",
}

// isInsufficientFundsStatus reports whether status and message together say the
// upstream account is out of funds. Only 402/403 qualify; other statuses that
// merely mention billing (e.g. a 429 quota window) keep their own handling.
func isInsufficientFundsStatus(status int, message string) bool {
	if status != http.StatusPaymentRequired && status != http.StatusForbidden {
		return false
	}
	lower := strings.ToLower(message)
	if lower == "" {
		return false
	}
	for _, marker := range fundsErrorMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isInsufficientFundsResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isInsufficientFundsStatus(err.StatusCode(), err.Message)
}

func isInsufficientFundsError(err error) bool {
	if err == nil {
		return false
	}
	return isInsufficientFundsStatus(statusCodeFromError(err), err.Error())
}

// markFundsExhaustedLocked blocks the auth for fundsExhaustedCooldown. It reports
// whether the block is new (so the caller logs it once).
func markFundsExhaustedLocked(auth *Auth, now time.Time) bool {
	if auth == nil {
		return false
	}
	already := auth.FundsExhaustedUntil.After(now)
	if !already {
		auth.FundsExhaustedUntil = now.Add(fundsExhaustedCooldown)
	}
	auth.StatusMessage = fundsExhaustedStatusMessage
	return !already
}

func logFundsExhausted(authID string, provider string, until time.Time, status int) {
	log.Warnf("credential %s (provider=%s) disabled until %s: upstream reports insufficient funds (HTTP %d); clear by topping up and reloading config or restarting",
		authID, provider, until.UTC().Format(time.RFC3339), status)
}

// ClearFundsCooldowns lifts every funds block, e.g. after a config reload. It
// returns the number of credentials that were blocked.
func (m *Manager) ClearFundsCooldowns() int {
	if m == nil {
		return 0
	}
	now := time.Now()
	var snapshots []*Auth
	m.mu.Lock()
	for _, auth := range m.auths {
		if auth == nil || auth.FundsExhaustedUntil.IsZero() {
			continue
		}
		auth.FundsExhaustedUntil = time.Time{}
		if auth.StatusMessage == fundsExhaustedStatusMessage {
			auth.StatusMessage = ""
		}
		auth.UpdatedAt = now
		snapshots = append(snapshots, auth.Clone())
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		for _, snapshot := range snapshots {
			m.scheduler.upsertAuth(snapshot)
		}
	}
	return len(snapshots)
}

// errorRetryRank orders failures by how useful they are to the caller as the
// final answer when every credential has failed. Higher wins.
//
//	0: credential-scoped (401/402/403/404): says nothing about the other credentials
//	1: any other error
//	2: retryable (transport failure, 408, 429, 5xx): the client can simply try again
func errorRetryRank(err error) int {
	if err == nil {
		return -1
	}
	status := statusCodeFromError(err)
	switch {
	case status == http.StatusUnauthorized, status == http.StatusPaymentRequired,
		status == http.StatusForbidden, status == http.StatusNotFound:
		return 0
	case status == 0, status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return 2
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Retryable {
		return 2
	}
	return 1
}

// preferRetryableError keeps the most retryable of prev and next. Equal ranks
// prefer next, which matches the previous "last error wins" behavior.
func preferRetryableError(prev, next error) error {
	if next == nil {
		return prev
	}
	if prev == nil || errorRetryRank(next) >= errorRetryRank(prev) {
		return next
	}
	return prev
}
