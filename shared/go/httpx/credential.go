package httpx

import (
	"crypto/subtle"
	"net/http"
)

// CredentialMatches reports whether a presented bearer credential equals the
// configured one.
//
// Two properties, and both are the reason this is one function rather than an
// inline `==` at each of the ~17 places that needed it (ai-review S9):
//
//   - Constant time. These are bearer credentials, and `==` returns on the first
//     wrong byte — that answers "how much of this token is right", not the question
//     it was asked. The practical risk on this platform is low (the internal token
//     is only reachable inside the compose network), but the inconsistency was
//     systematic: commerce already did it correctly and everyone else did not.
//   - Fail closed on an unconfigured value. A service started WITHOUT a credential
//     must refuse everyone rather than admit anyone presenting nothing, which is
//     what an empty-vs-empty comparison would do.
//
// Lifted from commerce's credentialMatches (staff_credential.go), which stays as
// the thin wrapper its two-header call site reads better with.
func CredentialMatches(presented, configured string) bool {
	if configured == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}

// HeaderCredentialMatches is CredentialMatches against a request header. The
// header name is spelled once per call site instead of twice.
func HeaderCredentialMatches(r *http.Request, header, configured string) bool {
	return CredentialMatches(r.Header.Get(header), configured)
}

// InternalToken is the header every service's internal surface authenticates
// with. A constant so a typo is a compile error rather than an open door.
const InternalToken = "X-Internal-Token"
