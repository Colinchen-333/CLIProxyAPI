// Package requestmeta carries trusted, process-local gateway request metadata.
package requestmeta

import "context"

type ownSessionKey struct{}

// WithOwnSession is set only by the own-model ingress after pseudonymization.
// It must never be populated directly from an untrusted inbound header.
func WithOwnSession(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, ownSessionKey{}, session)
}

func OwnSession(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	session, _ := ctx.Value(ownSessionKey{}).(string)
	return session
}
