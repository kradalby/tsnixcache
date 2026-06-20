package auth

import (
	"context"
	"net/http"
	"slices"

	apitype "tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// Cap is the Tailscale capability grant for tsnixcache.
type Cap struct {
	Push bool `json:"push"`
}

// CapName is the capability name used in Tailscale ACL grants.
const CapName tailcfg.PeerCapability = "dalby.cc/cap/tsnixcache"

// WhoIser is implemented by *local.Client and allows fakes in tests.
type WhoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// Middleware wraps h, enforcing:
//   - GET/HEAD/OPTIONS: always allowed (safe methods per RFC 9110 §9.2.1)
//   - All other methods: require Push cap in CapMap
//   - If WhoIs fails: 401
//   - If no push grant: 403
func Middleware(whoiser WhoIser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}

			who, err := whoiser.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil {
				http.Error(w, "could not identify Tailscale peer", http.StatusUnauthorized)
				return
			}

			if !HasPushCap(who.CapMap) {
				http.Error(w, "write access requires push grant", http.StatusForbidden)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// HasPushCap reports whether the cap map contains a push grant.
// Missing cap or malformed JSON → false (default deny).
func HasPushCap(cm tailcfg.PeerCapMap) bool {
	rules, _ := tailcfg.UnmarshalCapJSON[Cap](cm, CapName)
	return slices.ContainsFunc(rules, func(c Cap) bool {
		return c.Push
	})
}

// isSafeMethod reports whether the HTTP method is non-state-changing
// per RFC 9110 §9.2.1 and thus exempt from the push-grant check.
func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}
