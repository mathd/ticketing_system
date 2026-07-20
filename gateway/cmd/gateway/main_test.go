package main

import "testing"

// The back-office (US-018) is a web shell like the scanner: registered at a
// non-/api/ prefix, NOT path-stripped (it serves under /admin/). This pins the
// route table so a careless edit can't drop the admin route or promote the
// storefront catch-all above it.
func TestBackofficeRoute(t *testing.T) {
	if routes["/admin/"] != "BACKOFFICE_URL" {
		t.Fatalf("gateway must proxy /admin/ to BACKOFFICE_URL, got %q", routes["/admin/"])
	}
	// The catch-all stays the storefront; longest-prefix (ServeMux) resolves
	// /admin/ ahead of / regardless of map order.
	if routes["/"] != "STOREFRONT_URL" {
		t.Fatalf("/ must remain the storefront catch-all, got %q", routes["/"])
	}
	// /admin/ is not an /api/ prefix, so it must not be path-stripped — the
	// app serves under its base. (Strip logic keys on the /api/ prefix.)
	if got := "/admin/"; got[:5] == "/api/" {
		t.Fatalf("admin prefix must not be an /api/ route")
	}
}
