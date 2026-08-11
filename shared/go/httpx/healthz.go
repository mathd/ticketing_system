// Package httpx holds the platform's shared HTTP building blocks.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// NamedCheck is a readiness probe on a service dependency (DB, bus, ...).
type NamedCheck struct {
	Name  string
	Probe func() error
}

// Check builds a NamedCheck for Healthz.
func Check(name string, probe func() error) NamedCheck {
	return NamedCheck{Name: name, Probe: probe}
}

type healthzResponse struct {
	Status  string            `json:"status"`
	Service string            `json:"service"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// Healthz returns the platform-standard health handler: 200 {"status":"ok"}
// when every check passes, 503 {"status":"degraded"} otherwise, naming which
// check failed but never why. Every component (services, gateway, web shells)
// exposes this shape on /healthz.
//
// Why the reason does not travel: /healthz is unauthenticated by necessity —
// Compose, the gateway fan-out and any future load balancer all poll it without a
// credential — and a driver's error text carries DSN fragments, host names and
// schema detail. The check NAME is the whole operator signal ("db is down");
// the text goes to the log, where the reader is already trusted.
func Healthz(service string, checks ...NamedCheck) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := healthzResponse{Status: "ok", Service: service}
		if len(checks) > 0 {
			resp.Checks = make(map[string]string, len(checks))
		}
		code := http.StatusOK
		for _, c := range checks {
			if err := c.Probe(); err != nil {
				slog.ErrorContext(r.Context(), "health check failed", "service", service, "check", c.Name, "error", err)
				resp.Checks[c.Name] = "unhealthy"
				resp.Status = "degraded"
				code = http.StatusServiceUnavailable
			} else {
				resp.Checks[c.Name] = "ok"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(resp)
	})
}
