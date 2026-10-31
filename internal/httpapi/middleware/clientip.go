package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIPFrom replaces RemoteAddr with the address in header, so the limits
// see the client and not the proxy in front of Relay. An empty header leaves
// RemoteAddr alone. Set it only when a proxy you trust writes that header on
// every request: otherwise any client could pick the address it is limited by.
// A missing or invalid value keeps the socket address.
func ClientIPFrom(header string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if header == "" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if addr, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(header))); err == nil {
				r.RemoteAddr = net.JoinHostPort(addr.String(), "0")
			}
			next.ServeHTTP(w, r)
		})
	}
}
