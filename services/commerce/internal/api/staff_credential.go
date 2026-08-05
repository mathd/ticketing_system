package api

import (
	"crypto/subtle"
	"net/http"
)

// staffWriteHeader carries the back office's commerce credential (TKT-194).
//
// Distinct from X-Internal-Token on purpose. That one value opens every
// service's internal surface, and TKT-191 deliberately withheld it from the
// back office so an internet-facing SSR process could not spend it. This one
// opens exactly one operation — the staff refund — and the enumeration in
// staff_credential_test.go is what keeps that true.
const staffWriteHeader = "X-Commerce-Staff-Write-Token"

// WithStaffWriteCredential supplies the back office's credential.
//
// An option rather than a New parameter: New already takes six positional
// strings, and a seventh would be one more thing for a call site to pass in the
// wrong order — with a credential, silently.
func (s *Server) WithStaffWriteCredential(token string) *Server {
	s.staffWriteToken = token
	return s
}

// staffOrInternal reports whether the caller may drive a staff-reachable
// internal operation: either the shared internal token, or the back office's
// commerce credential.
//
// One helper, so no call site can invent a second answer — and both arms fail
// closed on an unconfigured value, because a service started without a
// credential must refuse everyone rather than admit anyone presenting nothing.
// Constant-time throughout: these are bearer credentials, and a comparison that
// returns early on the first wrong byte answers a different question than the
// one it was asked.
func (s *Server) staffOrInternal(r *http.Request) bool {
	return credentialMatches(r.Header.Get("X-Internal-Token"), s.token) ||
		credentialMatches(r.Header.Get(staffWriteHeader), s.staffWriteToken)
}

func credentialMatches(presented, configured string) bool {
	if configured == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
}
