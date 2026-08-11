package httpx

import (
	"net"
	"net/http"
	"strings"
)

// ClientIP is the source key for a per-source rate limiter.
//
// X-Forwarded-For is trustworthy here, and only because of how it arrives: the
// gateway's reverse proxy uses the Rewrite hook, which STRIPS the inbound
// X-Forwarded-* headers before the hook runs, and SetXForwarded then writes the
// connecting peer's address. A caller who forges the header through the gateway
// has it discarded, not appended to — verified against the real proxy in
// gateway/cmd/gateway/main_test.go.
//
// The last element is taken rather than the first, which matters if a future
// ingress ever APPENDS instead of replacing: the earlier entries are then
// caller-supplied and the last one is the only one the nearest proxy wrote.
// Taking the first is the classic bypass.
//
// The residual, stated: this is only as good as the gateway being the sole
// ingress. Every service's own port is published in the Compose profiles, so
// anyone who can reach one directly sets this header freely. That is a
// deployment property, not something this function can enforce.
//
// Lived in commerce until a second service needed a source key (ai-review S4).
// Shared rather than copied because everything above is the reason the function
// is written this exact way, and a copy carries the code without the reason.
func ClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
			return last
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Not a host:port — use it whole rather than collapsing every such caller
		// into one shared bucket, which would let one of them refuse the others.
		return r.RemoteAddr
	}
	return host
}
