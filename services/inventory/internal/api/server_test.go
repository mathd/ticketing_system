package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTransitionsRequireInternalCredential(t *testing.T) {
	s := New(nil, "secret")
	for _, token := range []string{"", "wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/holds/00000000-0000-0000-0000-000000000001/finalize?organizer_id=00000000-0000-0000-0000-000000000002", nil)
		req.Header.Set("X-Internal-Token", token)
		res := httptest.NewRecorder()
		s.Router().ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status=%d want=%d", token, res.Code, http.StatusUnauthorized)
		}
	}
}
