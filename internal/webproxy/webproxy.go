// Package webproxy exposes an authenticated reverse proxy so the iOS in-app
// browser can view HTTP servers an agent spins up on the box (e.g. `python -m
// http.server`, a Vite dev server) without opening any new firewall ports.
//
// Why a proxy rather than direct access: the WireGuard tunnel routes only
// 10.100.0.1, and UFW only opens the relay's own port (7682). Dev servers also
// usually bind 127.0.0.1, unreachable over the tunnel. The relay already owns
// an authenticated, tunnel-reachable port — so it connects to the local dev
// server on the client's behalf and streams the response back. The bearer token
// (checked by the relay before dispatching here) is the auth boundary; the
// client's custom URL-scheme handler attaches it to every sub-request.
//
// Requests are GET/POST/etc to /proxy/{port}/{path...}. Only ports in the
// ephemeral/dev range are allowed, and the relay's own ports (and Ollama) are
// excluded so the proxy can never be turned against the auth surface itself.
package webproxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

const prefix = "/proxy/"

// blockedPorts are never proxiable: the relay's own API + terminal, Ollama, and
// WireGuard. Proxying these would let an authenticated browser request reach the
// relay's auth surface or the model backend through a different door.
var blockedPorts = map[int]bool{
	7681:  true, // terminal websocket
	7682:  true, // relay API (this server)
	11434: true, // Ollama
	51820: true, // WireGuard
}

// Handler returns an http.HandlerFunc that proxies /proxy/{port}/{path...} to
// http://127.0.0.1:{port}/{path...}. The caller is responsible for applying
// bearer auth before invoking this (the relay wraps it in authed()).
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		port, rest, ok := parseTarget(r.URL.Path)
		if !ok {
			http.Error(w, "usage: /proxy/{port}/{path}", http.StatusBadRequest)
			return
		}
		if port < 1024 || port > 65535 || blockedPorts[port] {
			http.Error(w, "port not allowed", http.StatusForbidden)
			return
		}

		target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(port)}
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = target.Scheme
				req.URL.Host = target.Host
				req.Host = target.Host
				// rest is the path only (r.URL.Path excludes the query); req.URL's
				// RawQuery is preserved as-is by ReverseProxy.
				req.URL.Path = "/" + rest
				// Strip our bearer credential before it leaves to the local server —
				// the dev server has no business seeing the relay token.
				req.Header.Del("Authorization")
				// We are the origin from localhost's point of view.
				req.Header.Set("X-Forwarded-Host", req.Header.Get("Host"))
			},
			// Rewrite redirect Location headers so they stay inside /proxy/{port}/,
			// otherwise the browser would try to follow an absolute localhost URL
			// it can't reach.
			ModifyResponse: func(resp *http.Response) error {
				if loc := resp.Header.Get("Location"); loc != "" {
					resp.Header.Set("Location", rewriteLocation(loc, port))
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				slog.Warn("webproxy upstream error", "port", port, "err", err)
				http.Error(w, fmt.Sprintf("no server reachable on port %d", port), http.StatusBadGateway)
			},
		}
		// httputil.ReverseProxy transparently handles WebSocket upgrades (HMR /
		// live-reload) when the client sends Connection: Upgrade.
		proxy.ServeHTTP(w, r)
	}
}

// parseTarget splits "/proxy/8000/foo/bar" into (8000, "foo/bar"). The query
// string is not part of r.URL.Path, so rest is the path only.
// Returns ok=false if the prefix or port segment is missing/non-numeric.
func parseTarget(path string) (port int, rest string, ok bool) {
	if !strings.HasPrefix(path, prefix) {
		return 0, "", false
	}
	tail := strings.TrimPrefix(path, prefix)
	portStr, rest, _ := strings.Cut(tail, "/")
	p, err := strconv.Atoi(portStr)
	if err != nil || portStr == "" {
		return 0, "", false
	}
	return p, rest, true
}

// rewriteLocation maps a redirect target back through the proxy. Absolute
// localhost URLs are rewritten to /proxy/{port}/path; root-relative paths are
// prefixed; anything else (external absolute URLs) is left untouched.
func rewriteLocation(loc string, port int) string {
	if strings.HasPrefix(loc, "/proxy/") {
		return loc
	}
	if u, err := url.Parse(loc); err == nil && u.Host != "" {
		// Only rewrite redirects that point back at the same localhost server.
		if u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" {
			p := strings.TrimPrefix(u.Path, "/")
			out := fmt.Sprintf("%s%d/%s", prefix, port, p)
			if u.RawQuery != "" {
				out += "?" + u.RawQuery
			}
			return out
		}
		return loc // external redirect — leave as-is
	}
	// Root-relative path → keep it inside the proxy namespace.
	if strings.HasPrefix(loc, "/") {
		return fmt.Sprintf("%s%d%s", prefix, port, loc)
	}
	return loc
}
