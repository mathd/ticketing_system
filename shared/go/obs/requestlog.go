package obs

import (
	"log/slog"
	"net/http"
	"time"
)

// RequestLogger logs one structured line per request with the span's
// trace_id/span_id (via the context) — the correlation line the smoke
// suite asserts across services. Mount inside Middleware so the span
// already exists.
func RequestLogger(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying writer so http.ResponseController (and any
// interface-aware caller) reaches Flusher/Hijacker/ReaderFrom on it —
// streaming/SSE/websocket upgrades keep working through this middleware.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
