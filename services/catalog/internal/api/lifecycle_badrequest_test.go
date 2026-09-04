package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// These requests exercise the generated binder and request validator at runtime. The
// declarative contract rule in status_declaration_test.go checks the full operation set.
func TestLifecycleRejectionsAreDeclaredBadRequests(t *testing.T) {
	for _, path := range []string{
		"/seat-maps/not-a-uuid/publish",
		"/performances/not-a-uuid/publish",
		"/performances/not-a-uuid/archive",
		"/performances/not-a-uuid/close",
		"/performances/not-a-uuid/reopen",
		"/series/not-a-uuid/publish",
		"/series/not-a-uuid/archive",
		"/festivals/not-a-uuid/publish",
		"/festivals/not-a-uuid/archive",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := newEnv(t).do("POST", path, nil)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("malformed path UUID must be 400, got %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	t.Run("closeSlot invalid body", func(t *testing.T) {
		recorder := newEnv(t).do("POST", "/performances/"+uuid.NewString()+"/close",
			map[string]any{"reason": 123})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid SlotCloseRequest must be 400, got %d %s", recorder.Code, recorder.Body.String())
		}
	})
}
