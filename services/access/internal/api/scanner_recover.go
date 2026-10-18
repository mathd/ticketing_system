package api

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"

	"github.com/felixge/httpsnoop"

	"ticketing/shared/obs"
)

// recoveredBody is the one route-neutral body for a recovered panic (TKT-557,
// D15). Recovery covers all eight Access operations, not only the scanner, so
// the body names no route.
var recoveredBody = map[string]string{"error": "request failed"}

// staleEntityHeaders are the representation headers a handler can set before
// it panics. They describe a body that will not be sent, so recovery removes
// them before it writes the fixed JSON 500.
var staleEntityHeaders = []string{
	"Content-Length", "Content-Encoding", "Content-Disposition", "Content-Language",
	"Content-Range", "Content-Location", "ETag", "Last-Modified", "Location",
}

// recoverRequests is the outer edge of the Access Router (TKT-557, ADR-085 §8
// ticket 0). It sits outside the identity slot, the request validator and its
// security hook, the response validator and every handler, so a panic in any
// of them is recovered.
//
// Before final headers, the client gets a fixed JSON 500 instead of a dropped
// connection. A recovered 500 does NOT mean that nothing happened: a handler
// can panic after a store commit, so the work can exist without a usable
// reply. After final headers the original status and bytes are already on the
// wire, and a second body would corrupt them: recovery then aborts the
// response with http.ErrAbortHandler and does not claim that a 500 replaced
// the status.
//
// The log line is the fixed message with the stack. The raw panic value is
// never logged: it can carry a scanner token or a capability path.
func recoverRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent, hijacked bool
		tracked := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					// 1xx informational headers are not final; 101 is.
					if code >= 200 || code == http.StatusSwitchingProtocols {
						sent = true
					}
					next(code)
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(b []byte) (int, error) {
					sent = true
					return next(b)
				}
			},
			WriteString: func(next httpsnoop.WriteStringFunc) httpsnoop.WriteStringFunc {
				return func(s string) (int, error) {
					sent = true
					return next(s)
				}
			},
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(src io.Reader) (int64, error) {
					sent = true
					return next(src)
				}
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
				return func() {
					sent = true
					next()
				}
			},
			FlushError: func(next httpsnoop.FlushErrorFunc) httpsnoop.FlushErrorFunc {
				return func() error {
					sent = true
					return next()
				}
			},
			Hijack: func(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
				return func() (net.Conn, *bufio.ReadWriter, error) {
					conn, rw, err := next()
					if err == nil {
						hijacked = true
					}
					return conn, rw, err
				}
			},
		})
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(recovered)
			}
			logger := log
			if logger == nil {
				logger = slog.Default()
			}
			logger.ErrorContext(r.Context(), "access request panic",
				"method", r.Method, "path", obs.SanitizedPath(r.URL.Path), "stack", string(debug.Stack()))
			if sent || hijacked {
				panic(http.ErrAbortHandler)
			}
			for _, name := range staleEntityHeaders {
				w.Header().Del(name)
			}
			write(w, http.StatusInternalServerError, recoveredBody)
		}()
		next.ServeHTTP(tracked, r)
	})
}
