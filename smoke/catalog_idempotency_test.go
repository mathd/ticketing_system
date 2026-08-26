//go:build smoke

package smoke_test

// Catalog create idempotency, over real HTTP through the gateway (TKT-200).
//
// The store suite proves the mechanism against Postgres. This tier proves the
// WIRING: that the header a caller sends actually reaches the column, that a
// replay comes back as a 201 with the first resource rather than a second row,
// and that a reused key with different terms is the declared 409 rather than a
// 500 laundered by the response validator.
//
// Those are different claims. A store test cannot fail when the handler drops
// the header on the floor, and that is precisely the edit this file catches.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestCatalogCreateReplaysOnRepeatedKey(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	key := "replay-" + uuid.NewString()
	body := map[string]any{
		"name": map[string]string{"en": "Replay Fest", "fr": "Festival rejoué"},
	}

	code, first := postWithKey(t, catalog+"/events", key, body)
	if code != http.StatusCreated {
		t.Fatalf("first create: %d %s", code, first)
	}
	code, second := postWithKey(t, catalog+"/events", key, body)
	if code != http.StatusCreated {
		t.Fatalf("a repeat of an identical request must replay as 201, got %d: %s", code, second)
	}

	var a, b map[string]any
	if err := json.Unmarshal(first, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &b); err != nil {
		t.Fatal(err)
	}
	// The id is the whole point: same key, same resource. Derived from the
	// requirement ("a repeat returns the first result"), not from a run.
	if a["id"] != b["id"] {
		t.Fatalf("one key produced two events over HTTP: %v and %v", a["id"], b["id"])
	}
	if a["created_at"] != b["created_at"] {
		t.Fatalf("a replay must carry the ORIGINAL creation time: %v then %v", a["created_at"], b["created_at"])
	}
}

// TestCatalogCreateRefusesReusedKeyWithDifferentTerms pins the 409 end to end,
// including that it is a DECLARED response: validateServiceResponse inside
// postWithKey fails the test if the status is undocumented, which under ADR-028
// is what would otherwise reach production as a 500.
func TestCatalogCreateRefusesReusedKeyWithDifferentTerms(t *testing.T) {
	catalog := gatewayURL + "/api/catalog"
	key := "conflict-" + uuid.NewString()

	if code, body := postWithKey(t, catalog+"/events", key, map[string]any{
		"name": map[string]string{"en": "First Terms", "fr": "Premiers termes"},
	}); code != http.StatusCreated {
		t.Fatalf("first create: %d %s", code, body)
	}
	code, body := postWithKey(t, catalog+"/events", key, map[string]any{
		"name": map[string]string{"en": "Different Terms", "fr": "Termes différents"},
	})
	if code != http.StatusConflict {
		t.Fatalf("reusing a key for different terms must be 409, got %d: %s", code, body)
	}
}

// TestCatalogCreateRefusesMissingKey pins that the requirement is enforced and
// not merely documented. The refusal is the generated wrapper's, which is why
// this asserts 400 and names the header rather than expecting a handler message.
func TestCatalogCreateRefusesMissingKey(t *testing.T) {
	code, body := postJSON(t, gatewayURL+"/api/catalog/events", map[string]any{
		"name": map[string]string{"en": "Keyless", "fr": "Sans clé"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("a create with no Idempotency-Key must be refused, got %d: %s", code, body)
	}
}
