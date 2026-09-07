package helps

import (
	"container/list"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Cache transports, not clients: per-request timeouts and cookies stay independent.
// The key includes identity and route; credentials are never retained in cache keys.
var transportCache = struct {
	sync.Mutex
	entries map[transportCacheKey]*list.Element
	order   *list.List
}{entries: make(map[transportCacheKey]*list.Element), order: list.New()}

type transportCacheKey struct {
	digest        [32]byte
	base          *http.Transport
	anonymousAuth *cliproxyauth.Auth
}

type transportEntry struct {
	key  transportCacheKey
	rt   http.RoundTripper
	used time.Time
}

func transportKey(kind, route string, auth *cliproxyauth.Auth, base *http.Transport) transportCacheKey {
	var anonymousAuth *cliproxyauth.Auth
	identity := "anonymous"
	if auth != nil {
		switch {
		case auth.ID != "":
			identity = "id:" + auth.ID
		case auth.Index != "":
			identity = "index:" + auth.Index
		case auth.FileName != "":
			identity = "file:" + auth.FileName
		default:
			identity = "instance"
			anonymousAuth = auth
		}
		identity = auth.Provider + ":" + identity
	}
	return transportCacheKey{digest: sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", kind, route, identity))), base: base, anonymousAuth: anonymousAuth}
}

func sharedTransport(key transportCacheKey, build func() http.RoundTripper) http.RoundTripper {
	transportCache.Lock()
	now := time.Now()
	if e := transportCache.entries[key]; e != nil {
		entry := e.Value.(*transportEntry)
		entry.used = now
		transportCache.order.MoveToFront(e)
		transportCache.Unlock()
		return entry.rt
	}
	var evicted []http.RoundTripper
	for e := transportCache.order.Back(); e != nil; e = transportCache.order.Back() {
		entry := e.Value.(*transportEntry)
		if len(transportCache.entries) < 128 && now.Sub(entry.used) < 5*time.Minute {
			break
		}
		delete(transportCache.entries, entry.key)
		transportCache.order.Remove(e)
		evicted = append(evicted, entry.rt)
	}
	rt := build()
	if rt != nil {
		transportCache.entries[key] = transportCache.order.PushFront(&transportEntry{key: key, rt: rt, used: now})
	}
	transportCache.Unlock()
	// Never hold the cross-provider cache lock while closing network resources.
	for _, old := range evicted {
		if closer, ok := old.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
	return rt
}

// Retain burst capacity without limiting active requests or changing caller transports.
func retainBurstIdleConnections(transport *http.Transport) {
	transport.MaxIdleConns = 1024
	transport.MaxIdleConnsPerHost = 1024
}

func sharedProxyTransport(route string, auth *cliproxyauth.Auth) *http.Transport {
	rt := sharedTransport(transportKey("proxy", route, auth, nil), func() http.RoundTripper {
		transport := buildProxyTransport(route)
		if transport == nil {
			return nil
		}
		retainBurstIdleConnections(transport)
		return transport
	})
	transport, _ := rt.(*http.Transport)
	return transport
}

// SharedHTTP11Transport preserves the source route while reusing its HTTP/1 pool.
// Context-injected transports are partitioned by their pointer as well as auth.
func SharedHTTP11Transport(base *http.Transport, auth *cliproxyauth.Auth) *http.Transport {
	if base == nil {
		base, _ = http.DefaultTransport.(*http.Transport)
	}
	return sharedTransport(transportKey("http11", "", auth, base), func() http.RoundTripper {
		var clone *http.Transport
		if base == nil {
			clone = &http.Transport{IdleConnTimeout: 90 * time.Second}
		} else {
			clone = base.Clone()
		}
		retainBurstIdleConnections(clone)
		clone.ForceAttemptHTTP2 = false
		clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		if clone.TLSClientConfig == nil {
			clone.TLSClientConfig = &tls.Config{}
		} else {
			clone.TLSClientConfig = clone.TLSClientConfig.Clone()
		}
		clone.TLSClientConfig.NextProtos = []string{"http/1.1"}
		return clone
	}).(*http.Transport)
}
