// Copyright (c) 2026 Kristoffer Dalby
// SPDX-License-Identifier: BSD-3-Clause

// Package auth gates HTTP writes behind a Tailscale push capability grant.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
	apitype "tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

// Cap is the Tailscale capability grant for tsnixcache.
type Cap struct {
	Push bool `json:"push"`
}

// CapName is the capability name used in Tailscale ACL grants.
const CapName tailcfg.PeerCapability = "kradalby.no/cap/tsnixcache"

// WhoIser is implemented by *local.Client and allows fakes in tests.
type WhoIser interface {
	WhoIs(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)
}

// Reject reasons, used as the sole label of rejectsTotal.
const (
	reasonUnidentified = "unidentified"
	reasonNoGrant      = "no_grant"
	// reasonLocalWrite is a write on a listener that carries no identity at
	// all, refused by ReadOnly. Distinct from reasonUnidentified: there the
	// peer could have been identified and was not, here there was never
	// anything to ask.
	reasonLocalWrite = "local_write"
)

// LocalWriteFlag is the flag that turns writes back on for a listener with no
// identity. Named in the refusal itself, because the operator who meets that
// 403 is not reading this source.
const LocalWriteFlag = "--local-write"

// ReadOnlyMessage is the body of that refusal.
const ReadOnlyMessage = "this listener has no authentication, so it is read-only;" +
	" restart tsnixcache with " + LocalWriteFlag + " to allow writes here"

// metricRejects is the name of the rejection counter, shared with its tests.
const metricRejects = "tsnixcache_auth_rejects_total"

// rejectsTotal counts writes refused at the push-grant boundary. The only
// label is the reason: peer identity and store path are unbounded, so they
// belong in the log line, not in a time series.
var rejectsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: metricRejects,
		Help: "Write requests refused at the push-grant boundary, by reason.",
	},
	[]string{"reason"},
)

// A CounterVec creates its children lazily, so a server that has never
// rejected a write exports no series at all — an operator cannot tell "nothing
// rejected" from "scrape broken", and absent()-style alerts misfire. Touch
// every known reason so a clean server reports zeros.
func init() {
	rejectsTotal.WithLabelValues(reasonUnidentified)
	rejectsTotal.WithLabelValues(reasonNoGrant)
	rejectsTotal.WithLabelValues(reasonLocalWrite)
}

// Collector returns the auth metrics, for the caller to register on the
// registry it serves /metrics from.
func Collector() prometheus.Collector {
	return rejectsTotal
}

// Middleware wraps h, enforcing:
//   - GET/HEAD/OPTIONS: always allowed (safe methods per RFC 9110 §9.2.1)
//   - All other methods: require Push cap in CapMap
//   - If WhoIs fails or identifies nobody: 401
//   - If no push grant: 403
func Middleware(whoiser WhoIser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)

				return
			}

			who, err := whoiser.WhoIs(r.Context(), r.RemoteAddr)
			// localapi decodes a JSON null body into (nil, nil), so a nil
			// response without an error is a contractual outcome, not just a
			// test artefact. Treating it as an error keeps a localapi hiccup a
			// counted 401 instead of a nil dereference in the handler.
			if err != nil || who == nil {
				// A misconfigured grant is otherwise invisible: the pusher sees
				// only an HTTP status, so say who was turned away and why.
				rejectsTotal.WithLabelValues(reasonUnidentified).Inc()
				slog.Warn("auth: write rejected, peer not identified",
					"remote", r.RemoteAddr, "method", r.Method, "path", LogPath(r.URL.Path), "err", err)
				http.Error(w, "could not identify Tailscale peer", http.StatusUnauthorized)

				return
			}

			if !HasPushCap(who.CapMap) {
				rejectsTotal.WithLabelValues(reasonNoGrant).Inc()
				slog.Warn("auth: write rejected, no push grant",
					"remote", r.RemoteAddr, "node", nodeName(who), "user", userName(who),
					"method", r.Method, "path", LogPath(r.URL.Path), "cap", CapName)
				http.Error(w, "write access requires push grant", http.StatusForbidden)

				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ReadOnly wraps h for a listener that carries no identity, refusing with 403
// anything that is not a safe method, plus the handful of paths that change
// state whatever method reaches them (see isStateChangingPath).
//
// Middleware is the identity boundary, and it only exists where there is an
// identity to ask about: a tsnet connection, which WhoIs can attribute to a
// node. A plain --listen socket has no such answer, so it used to serve the
// bare handler — every uid on the host, and anything else that could reach the
// address, could PUT a NAR and a narinfo, have the server import it as a nix
// trusted-user, and have it re-signed with the cache's own key. At the default
// priority of 30 that path then shadows cache.nixos.org for every client that
// trusts the key.
//
// Reads are untouched: serving a substituter over loopback is what these
// listeners are for. Writes are refused unless the operator opts back in with
// LocalWriteFlag, which is expressed by not applying this wrapper at all —
// whether a handler is wrapped is the whole of the policy, so there is no
// second place for the two to disagree.
func ReadOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) && !isStateChangingPath(r.URL.Path) {
			next.ServeHTTP(w, r)

			return
		}

		rejectsTotal.WithLabelValues(reasonLocalWrite).Inc()
		slog.Warn("auth: write rejected, listener has no authentication",
			"remote", r.RemoteAddr, "method", r.Method, "path", LogPath(r.URL.Path), "flag", LocalWriteFlag)
		http.Error(w, ReadOnlyMessage, http.StatusForbidden)
	})
}

// HasPushCap reports whether the cap map contains a push grant.
// Missing cap or malformed JSON → false (default deny).
func HasPushCap(cm tailcfg.PeerCapMap) bool {
	rules, _ := tailcfg.UnmarshalCapJSON[Cap](cm, CapName)

	return slices.ContainsFunc(rules, func(c Cap) bool {
		return c.Push
	})
}

// maxLogPath bounds the request path in a log line. A real path is
// "/nar/<32 chars>.nar" or a narinfo name; anything longer says all it needs to.
const maxLogPath = 128

// LogPath returns a request path truncated for logging. The path is whatever
// the peer sent — up to the server's header limit — so without this a single
// request can put a megabyte in the journal, needing no grant to do it. Exported
// because the access log has the same exposure as this package's rejection log.
// How often it can be logged is left to journald's rate limiting.
func LogPath(p string) string {
	if len(p) > maxLogPath {
		return p[:maxLogPath] + "…truncated"
	}

	return p
}

// nodeName returns the peer's node name for logging. WhoIs fills Node in on
// success, but it is a pointer, so guard it.
func nodeName(who *apitype.WhoIsResponse) string {
	if who == nil || who.Node == nil {
		return ""
	}

	return who.Node.Name
}

// userName returns the peer's login name for logging.
func userName(who *apitype.WhoIsResponse) string {
	if who == nil || who.UserProfile == nil {
		return ""
	}

	return who.UserProfile.LoginName
}

// isStateChangingPath reports whether a path changes server state regardless of
// the method used to reach it.
//
// tsweb's debug index links the force-GC endpoint as an ordinary anchor, so the
// button sends GET and its handler ignores the method entirely. A method rule
// therefore cannot see it, and a listener advertised as read-only would still
// let anything that can reach it force a stop-the-world collection per request.
// Introspection under /debug (pprof, expvar) is deliberately not listed: it is
// read-only, and leaving it reachable on a local socket is what makes profiling
// a running cache possible.
func isStateChangingPath(p string) bool {
	return path.Clean(p) == "/debug/gc"
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
