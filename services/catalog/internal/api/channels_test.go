package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// The sales-channel registry's HTTP surface (TKT-235 / epic TKT-17).
//
// Every request goes through env.do / env.doWithHeaders so it passes the real
// router, the request validator and the ADR-028 response validator, and so the
// 2xx coverage gate sees it. A direct handler call would satisfy neither.

// operatorGet reaches the hand-mounted /internal/ operator reads.
//
// Not env.do with a staff credential: catalog's contract cannot express a
// staff-authenticated GET (TKT-191's invariant — safe operations must opt out of
// the write credential), so the operator reads live on /internal/ behind
// X-Internal-Token, like catalog's other guarded reads. newEnv configures the
// token as "test-internal-token".
func operatorGet(e *env, path string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.doWithHeaders(http.MethodGet, path, nil, map[string]string{"X-Internal-Token": testInternalToken})
}

// operatorListChannels reads the channel registry as the BACK OFFICE does
// (TKT-245): the staff-write credential plus an assertion naming the organizer.
//
// The organizer is no longer a query parameter, so this helper is the only way
// the list can be reached for a given tenant — which is the point. `/internal/
// channels/{id}` is unaffected and still takes the internal token.
func operatorListChannels(e *env, organizerID uuid.UUID) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.doWithHeaders(http.MethodGet, "/internal/channels", nil, map[string]string{
		staffWriteHeader:         testStaffWriteToken,
		organizerAssertionHeader: e.assertionFor(organizerID),
	})
}

// testInternalToken is the internal credential newEnv configures.
const testInternalToken = "test-internal-token"

// channelWrite issues a channel write ACTING FOR organizerID.
//
// Before TKT-245 the organizer was a field in the body and this helper built it
// there; now it travels in the assertion and cannot be written into the body at
// all — `ChannelCreate` is additionalProperties: false, so a submitted
// organizer_id is refused by the validator rather than ignored.
func channelWrite(e *env, method, path string, organizerID uuid.UUID, body map[string]any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.doWithHeaders(method, path, body, map[string]string{
		staffWriteHeader:         testStaffWriteToken,
		organizerAssertionHeader: e.assertionFor(organizerID),
	})
}

func createChannelBody(code, name, kind string, enabled *bool) map[string]any {
	body := map[string]any{
		"code":         code,
		"display_name": name,
		"kind":         kind,
	}
	if enabled != nil {
		body["enabled"] = *enabled
	}
	return body
}

func TestCreateChannelReturnsTheDefinition(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()

	rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("pos", "Box office", "pos", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /channels = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Code != "pos" || got.DisplayName != "Box office" || got.Kind != "pos" {
		t.Fatalf("got %+v, want code=pos display_name='Box office' kind=pos", got)
	}
	// Omitted `enabled` means enabled: a channel is created sellable unless said
	// otherwise. If this ever flips, channels would be created invisible to the
	// public read and an operator would see an empty storefront selector.
	//
	// Note what actually enforces this — see TestChannelEnabledDefaultLivesInTheContract.
	// The request validator materializes the schema's `default: true` before the
	// handler runs, so this assertion is really testing the CONTRACT. Changing
	// the handler alone cannot break it, which is why the contract itself is
	// pinned separately.
	if !got.Enabled {
		t.Fatal("enabled = false when the field was omitted, want true")
	}
	if got.OrganizerId != org {
		t.Fatalf("organizer_id = %s, want %s", got.OrganizerId, org)
	}
}

func TestCreateChannelHonoursExplicitlyDisabled(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()
	disabled := false

	rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("partner-a", "Partner A", "reseller", &disabled))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /channels = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled {
		t.Fatal("enabled = true when the body said false — an explicit false must not be read as absent")
	}
}

func TestChannelCodeIsUniquePerOrganizerNotGlobally(t *testing.T) {
	e := newEnv(t)
	orgA, orgB := uuid.New(), uuid.New()

	if rec := channelWrite(e, http.MethodPost, "/channels", orgA, createChannelBody("pos", "A box office", "pos", nil)); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	// Same organizer, same code: refused.
	rec := channelWrite(e, http.MethodPost, "/channels", orgA, createChannelBody("pos", "Duplicate", "pos", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate code for the same organizer = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	// Different organizer, same code: allowed. Tenants do not share a namespace.
	if rec := channelWrite(e, http.MethodPost, "/channels", orgB, createChannelBody("pos", "B box office", "pos", nil)); rec.Code != http.StatusCreated {
		t.Fatalf("same code for a different organizer = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func TestChannelCodesDifferingOnlyByCaseOrSpaceAreDistinctChannels(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()

	// ADR-024: codes are exact opaque strings. Four columns in three services
	// store them verbatim, so a registry that folded case or trimmed space would
	// disagree with all four. These are three different channels, and creating
	// all three must succeed — a 409 here means something normalized.
	for _, code := range []string{"pos", "POS", "pos "} {
		rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody(code, "Box office", "pos", nil))
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST /channels code=%q = %d, want 201 (codes are exact, not normalized): %s", code, rec.Code, rec.Body.String())
		}
		var got Channel
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Code != code {
			t.Fatalf("stored code = %q, want %q verbatim", got.Code, code)
		}
	}
}

func TestUpdateChannelRefusesToRenameTheCode(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()

	rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("legacy-pos", "Old box office", "pos", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// A different code is a rename, and a rename would orphan the code already
	// recorded on live claims, fee rules and split schedules — none of which
	// reference this table, so nothing would cascade and nothing would complain.
	rec = channelWrite(e, http.MethodPut, "/channels/"+created.Id.String(), org, map[string]any{
		"code": "pos", "display_name": "Old box office", "kind": "pos", "enabled": true,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("renaming the code = %d, want 409: %s", rec.Code, rec.Body.String())
	}

	// And the stored row is untouched.
	rec = operatorGet(e, "/internal/channels/"+created.Id.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("GET after refused rename = %d: %s", rec.Code, rec.Body.String())
	}
	var after Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.Code != "legacy-pos" {
		t.Fatalf("code = %q after a refused rename, want %q — the refusal must not partially apply", after.Code, "legacy-pos")
	}
}

func TestUpdateChannelChangesTheMutableFields(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()

	rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("web", "Website", "web", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rec = channelWrite(e, http.MethodPut, "/channels/"+created.Id.String(), org, map[string]any{
		"code": "web", "display_name": "Main website", "kind": "web", "enabled": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DisplayName != "Main website" || got.Enabled {
		t.Fatalf("got display_name=%q enabled=%v, want 'Main website' / false", got.DisplayName, got.Enabled)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("updated_at %v not after created_at %v — the store must write it explicitly; catalog has no trigger",
			got.UpdatedAt, got.CreatedAt)
	}
}

func TestUpdateUnknownChannelIs404NotAnImmutabilityConflict(t *testing.T) {
	e := newEnv(t)
	// Reporting 409 for an id that does not exist would tell a caller that an id
	// it guessed is real.
	rec := channelWrite(e, http.MethodPut, "/channels/"+uuid.New().String(), uuid.New(), map[string]any{
		"code": "pos", "display_name": "Box office", "kind": "pos", "enabled": true,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("PUT to an unknown channel = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestGetUnknownChannelIs404(t *testing.T) {
	e := newEnv(t)
	if rec := operatorGet(e, "/internal/channels/"+uuid.New().String()); rec.Code != http.StatusNotFound {
		t.Fatalf("GET unknown channel = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// The load-bearing read test: the operator sees disabled channels, the public
// does not.
func TestPublicReadShowsOnlyEnabledChannelsAndOperatorReadShowsBoth(t *testing.T) {
	e := newEnv(t)
	org, other := uuid.New(), uuid.New()
	disabled := false

	seed := []struct {
		organizer uuid.UUID
		code      string
		name      string
		kind      string
		enabled   *bool
	}{
		{org, "web", "Website", "web", nil},
		{org, "pos", "Box office", "pos", nil},
		{org, "retired", "Retired partner", "reseller", &disabled},
		{other, "web", "Someone else's website", "web", nil},
	}
	for _, s := range seed {
		if rec := channelWrite(e, http.MethodPost, "/channels", s.organizer, createChannelBody(s.code, s.name, s.kind, s.enabled)); rec.Code != http.StatusCreated {
			t.Fatalf("seed %q = %d: %s", s.code, rec.Code, rec.Body.String())
		}
	}

	// Public: enabled only, this organizer only.
	rec := e.do(http.MethodGet, "/public/channels?organizer_id="+org.String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /public/channels = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var pub PublicChannelList
	if err := json.Unmarshal(rec.Body.Bytes(), &pub); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotPublic := make([]string, 0, len(pub.Channels))
	for _, c := range pub.Channels {
		gotPublic = append(gotPublic, c.Code)
	}
	if len(gotPublic) != 2 || gotPublic[0] != "pos" || gotPublic[1] != "web" {
		t.Fatalf("public channels = %v, want [pos web] — disabled and other-organizer rows must not appear", gotPublic)
	}

	// Operator: everything for this organizer, including the disabled one.
	rec = operatorListChannels(e, org)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /channels = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var ops ChannelList
	if err := json.Unmarshal(rec.Body.Bytes(), &ops); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gotOps := make([]string, 0, len(ops.Channels))
	for _, c := range ops.Channels {
		gotOps = append(gotOps, c.Code)
	}
	if len(gotOps) != 3 {
		t.Fatalf("operator channels = %v, want 3 including the disabled one", gotOps)
	}
	sawRetired := false
	for _, c := range ops.Channels {
		if c.Code == "retired" {
			sawRetired = true
			if c.Enabled {
				t.Fatal("the retired channel reports enabled=true")
			}
		}
		if c.OrganizerId != org {
			t.Fatalf("operator list leaked organizer %s", c.OrganizerId)
		}
	}
	if !sawRetired {
		t.Fatal("operator list omitted the disabled channel — that read exists to show it")
	}
}

func TestPublicChannelListDeclaresTheMinutesTier(t *testing.T) {
	e := newEnv(t)
	rec := e.do(http.MethodGet, "/public/channels?organizer_id="+uuid.New().String(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /public/channels = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != CacheControlPublicReads {
		t.Fatalf("Cache-Control = %q, want %q (ADR-004 minutes tier)", got, CacheControlPublicReads)
	}
	// No Age header: this read is not served from catalog's in-memory public-read
	// cache, so nothing has aged. Age is required only where a response can come
	// back already stale.
	if got := rec.Header().Get("Age"); got != "" {
		t.Fatalf("Age = %q, want absent — /public/channels is not cached in memory", got)
	}
}

func TestOperatorChannelReadsAreNotShareableCached(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()
	rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("pos", "Box office", "pos", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The two operator reads take different credentials now (TKT-245): the list is
	// organizer-scoped and needs an assertion, the single-row read is not and
	// still takes the internal token alone. Both must be never-cacheable.
	for _, tc := range []struct {
		path string
		get  func() *httptest.ResponseRecorder
	}{
		{"/internal/channels", func() *httptest.ResponseRecorder { return operatorListChannels(e, org) }},
		{"/internal/channels/" + created.Id.String(), func() *httptest.ResponseRecorder {
			return operatorGet(e, "/internal/channels/"+created.Id.String())
		}},
	} {
		path := tc.path
		rec := tc.get()
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != cacheControlNever {
			t.Fatalf("GET %s Cache-Control = %q, want %q — operator config carries disabled rows and is never shared-cacheable", path, got, cacheControlNever)
		}
	}
}

func TestChannelWritesRequireTheStaffCredential(t *testing.T) {
	e := newEnv(t)
	rec := e.doWithHeaders(http.MethodPost, "/channels",
		createChannelBody("pos", "Box office", "pos", nil),
		map[string]string{staffWriteHeader: "wrong-token"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /channels with a bad credential = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestOperatorChannelReadsRequireTheInternalCredential(t *testing.T) {
	e := newEnv(t)
	// The two operator reads are guarded by guardInternalSurface; the public one
	// is not. A read that only passes because a credential happened to be
	// attached would hide a public surface silently acquiring — or losing — a
	// guard.
	for _, path := range []string{"/internal/channels", "/internal/channels/" + uuid.New().String()} {
		if rec := e.do(http.MethodGet, path, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s = %d, want 401: %s", path, rec.Code, rec.Body.String())
		}
	}
	// And the public read stays open.
	if rec := e.do(http.MethodGet, "/public/channels?organizer_id="+uuid.New().String(), nil); rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated GET /public/channels = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestChannelWriteGateRefusesBadInput(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name string
		body map[string]any
	}{
		{"unknown kind", createChannelBody("x", "X", "partner", nil)},
		{"empty code", createChannelBody("", "X", "web", nil)},
		{"empty display name", createChannelBody("x", "", "web", nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, "/channels", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST /channels = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// The `enabled` default is the CONTRACT'S, and this is what pins it.
//
// Written after a mutation check found the gap: flipping the handler's
// `enabled := true` to false left every behavioural test green, because
// kin-openapi materializes schema defaults into the body before the handler
// runs. The behavioural test therefore could not distinguish "the handler
// defaults correctly" from "the contract does" — and only one of them is true.
// Asserting on the document is the only place the real mechanism is visible.
func TestChannelEnabledDefaultLivesInTheContract(t *testing.T) {
	doc := loadSpec(t)
	schema, ok := doc.Components.Schemas["ChannelCreate"]
	if !ok {
		t.Fatal("ChannelCreate schema missing from the contract")
	}
	enabled, ok := schema.Value.Properties["enabled"]
	if !ok {
		t.Fatal("ChannelCreate.enabled missing from the contract")
	}
	if enabled.Value.Default == nil {
		t.Fatal("ChannelCreate.enabled declares no default — an omitted field would then create a DISABLED channel, invisible to the public read")
	}
	if got, want := enabled.Value.Default, true; got != want {
		t.Fatalf("ChannelCreate.enabled default = %v, want %v", got, want)
	}
	// And it must stay optional: making it required would turn an omitted field
	// into a 400 rather than a sensible default.
	for _, req := range schema.Value.Required {
		if req == "enabled" {
			t.Fatal("ChannelCreate.enabled is required; it must stay optional for the default to mean anything")
		}
	}
}

// The back office reaches the operator channel LIST with the staff-write
// credential it already holds (TKT-236 / ADR-053).
//
// Narrow by construction, and the narrowness is the decision. The back office
// needs one read — the collection — to show disabled channels on its admin page.
// Everything else on /internal/ stays shared-token-only, including the
// single-row read beside it, because nothing asked for those.
//
// Why this adds no blast radius: X-Catalog-Staff-Write-Token already opens every
// unsafe catalog operation, INCLUDING createChannel and updateChannel. A holder
// that can create and update channels can already learn which ones exist, so
// letting it list them grants nothing it could not obtain by other means. That
// is the whole argument, and it does NOT transfer to inventory (TKT-244), where
// no such credential exists and the surface is a capacity write.
//
// What this is NOT: tenant isolation. organizer_id is caller-supplied and
// catalog authenticates the DEPUTY PROCESS, not the staff member (ADR-021 — name
// the adversary). The back office passes its session's organizer and never one
// from the request; catalog cannot enforce that and does not claim to.
func TestOperatorChannelListAcceptsTheStaffWriteCredential(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()
	if rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("pos", "Box office", "pos", nil)); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", rec.Code, rec.Body.String())
	}

	rec := e.doWithHeaders(http.MethodGet, "/internal/channels", nil, map[string]string{
		staffWriteHeader:         testStaffWriteToken,
		organizerAssertionHeader: e.assertionFor(org),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /internal/channels with the staff-write credential = %d, want 200: %s",
			rec.Code, rec.Body.String())
	}
	var out ChannelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Channels) != 1 || out.Channels[0].Code != "pos" {
		t.Fatalf("channels = %+v, want the one seeded channel", out.Channels)
	}
}

// The allowance is scoped to ONE method on ONE path. Everything else on the
// internal surface still requires the shared token.
//
// This is the test that keeps the allowance narrow. Without it, a future edit
// widening `guardInternalSurface` to accept the staff token generally would pass
// every other test in this file — and would hand a public-facing SSR process the
// whole internal surface, which is exactly what compose.yaml refuses it.
func TestTheStaffWriteCredentialOpensNothingElseOnTheInternalSurface(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()
	rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("pos", "Box office", "pos", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d", rec.Code)
	}
	var created Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	staff := map[string]string{staffWriteHeader: testStaffWriteToken}
	refused := []struct {
		name, method, path string
	}{
		// The sibling read. Deliberately NOT opened: the page does not need it.
		{"single channel read", http.MethodGet, "/internal/channels/" + created.Id.String()},
		// Catalog's other internal reads, none of which this ticket touches.
		{"ticket type", http.MethodGet, "/internal/ticket-types/" + uuid.New().String()},
		{"published performance", http.MethodGet, "/internal/performances/" + uuid.New().String()},
		{"pool offer state", http.MethodGet, "/internal/pools/" + uuid.New().String() + "/offer-state"},
		{"seat map pins", http.MethodGet, "/internal/seat-map-pins"},
		// The cache kill-switch — a control surface, and the one whose accidental
		// exposure would be worst.
		{"cache control status", http.MethodGet, "/internal/cache-control"},
		// A DIFFERENT METHOD on the very path that is allowed. The allowance is
		// per method+path, not per path.
		{"channels collection, wrong method", http.MethodPut, "/internal/channels"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			rec := e.doWithHeaders(tc.method, tc.path, nil, staff)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s with only the staff-write credential = %d, want 401 — "+
					"the allowance is one method on one path, and a public-facing SSR process "+
					"must not reach the rest of the internal surface", tc.method, tc.path, rec.Code)
			}
		})
	}
}

// The shared internal token still works on the route the staff token now also
// opens. This is an ADDITIONAL accepted credential, not a replacement: access
// and other services reach catalog's internal surface with it.
// The channel LIST now needs an organizer assertion whatever credential opened
// the door (TKT-245) — including the shared internal token.
//
// This reverses what this test asserted before, and the reversal is deliberate
// rather than incidental, so it is written down here.
//
// The internal token authenticates a SERVICE. It names no tenant, and the
// organizer used to arrive as a query parameter, which is exactly the shape this
// ticket removes: a caller-chosen organizer that catalog had nothing to check
// against. Keeping a query-parameter path "for services" would have left the
// enumeration capability open to anyone holding the shared token — the widest
// credential in the system — while closing it for the narrow one. That is
// backwards.
//
// Verified before removing it: `GET /internal/channels` has exactly ONE caller
// repo-wide, the back office (web/backoffice/src/lib/catalog.ts), which does hold
// an assertion. No Go service reads this route, so nothing lost a capability it
// was using. The sibling `/internal/channels/{id}` is untouched and still takes
// the internal token alone — it names a specific row rather than enumerating a
// tenant's configuration.
//
// If a service ever does need this list, it needs an organizer-bound credential
// of its own (ADR-056's shape), not a query parameter restored.
func TestOperatorChannelListRequiresAnAssertionEvenWithTheInternalToken(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()
	if rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("pos", "Box office", "pos", nil)); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d", rec.Code)
	}

	// The internal token alone: through the prefix guard, refused at the handler.
	if rec := operatorGet(e, "/internal/channels"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET with X-Internal-Token and no assertion = %d, want 401 — the token names no "+
			"tenant, so it must not be able to enumerate one: %s", rec.Code, rec.Body.String())
	}

	// The internal token WITH an assertion: allowed, and scoped to the assertion.
	rec := e.doWithHeaders(http.MethodGet, "/internal/channels", nil, map[string]string{
		"X-Internal-Token":       testInternalToken,
		organizerAssertionHeader: e.assertionFor(org),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET with X-Internal-Token + assertion = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list ChannelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Channels) != 1 || list.Channels[0].OrganizerId != org {
		t.Fatalf("the read must be scoped to the assertion's organizer, got %+v", list.Channels)
	}
}

// A wrong staff credential is refused, and indistinguishably from an absent one.
func TestOperatorChannelListRefusesAWrongStaffCredential(t *testing.T) {
	e := newEnv(t)
	path := "/internal/channels"
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"wrong staff token", map[string]string{staffWriteHeader: "not-the-credential"}},
		{"empty staff token", map[string]string{staffWriteHeader: ""}},
		{"no credential at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := e.doWithHeaders(http.MethodGet, path, nil, tc.headers); rec.Code != http.StatusUnauthorized {
				t.Fatalf("= %d, want 401", rec.Code)
			}
		})
	}
}

// The GUARD's allowance is exactly one method+path — asserted against the guard
// itself, not through a route.
//
// Why this test exists rather than trusting the route-level ones above: the
// hand-mounted handlers carry their own credential check, so a widened
// guardInternalSurface is INVISIBLE from outside. Open the guard to
// `/internal/channels/{id}` and `getChannel`'s own internalAuthorized still
// answers 401 — the status is unchanged, every route test stays green, and the
// prefix guard has quietly stopped being narrow. A mutation check found exactly
// that: widening the match to a prefix killed no test.
//
// Defence in depth is why the widening is not immediately exploitable. It is
// also why it cannot be detected through the front door. So this asserts the
// predicate directly.
func TestStaffCredentialAllowanceIsExactlyOneMethodAndPath(t *testing.T) {
	s := NewServer(newFakeStore(), &fakePublisher{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test-internal-token", testStaffWriteToken)

	with := func(method, path, token string) *http.Request {
		req := httptest.NewRequest(method, "http://catalog.local"+path, nil)
		if token != "" {
			req.Header.Set(staffWriteHeader, token)
		}
		return req
	}

	if !s.staffMayReadOperatorChannels(with(http.MethodGet, "/internal/channels", testStaffWriteToken)) {
		t.Fatal("GET /internal/channels with the staff credential is not allowed — the one route this ticket opens")
	}
	// A query string must not change the decision: r.URL.Path excludes it, and
	// the real caller always sends ?organizer_id=...
	if !s.staffMayReadOperatorChannels(with(http.MethodGet, "/internal/channels?organizer_id="+uuid.New().String(), testStaffWriteToken)) {
		t.Fatal("a query string defeated the path match")
	}

	refused := []struct{ name, method, path, token string }{
		// The sibling read, one character away. THIS is the case a prefix match
		// would silently include, and the reason this test is written against
		// the predicate.
		{"sibling single-channel read", http.MethodGet, "/internal/channels/" + uuid.New().String(), testStaffWriteToken},
		{"trailing slash", http.MethodGet, "/internal/channels/", testStaffWriteToken},
		{"a longer path that shares the prefix", http.MethodGet, "/internal/channels-secret", testStaffWriteToken},
		// Other methods on the allowed path.
		{"PUT", http.MethodPut, "/internal/channels", testStaffWriteToken},
		{"POST", http.MethodPost, "/internal/channels", testStaffWriteToken},
		{"DELETE", http.MethodDelete, "/internal/channels", testStaffWriteToken},
		// Other internal routes entirely.
		{"cache control", http.MethodGet, "/internal/cache-control", testStaffWriteToken},
		{"seat map pins", http.MethodGet, "/internal/seat-map-pins", testStaffWriteToken},
		// Wrong or absent credential on the allowed route.
		{"wrong token", http.MethodGet, "/internal/channels", "not-the-credential"},
		{"no token", http.MethodGet, "/internal/channels", ""},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if s.staffMayReadOperatorChannels(with(tc.method, tc.path, tc.token)) {
				t.Fatalf("%s %s was ALLOWED by the staff-credential exception — it must open "+
					"exactly GET /internal/channels and nothing else", tc.method, tc.path)
			}
		})
	}
}

// A server built with no staff credential configured must not accept a request
// that also sends nothing. Empty == empty is the classic open door, and the
// route tests cannot see it because newEnv always configures a credential.
func TestStaffCredentialAllowanceFailsClosedWhenUnconfigured(t *testing.T) {
	s := NewServer(newFakeStore(), &fakePublisher{},
		slog.New(slog.NewTextHandler(io.Discard, nil)), "test-internal-token", "")
	req := httptest.NewRequest(http.MethodGet, "http://catalog.local/internal/channels", nil)
	req.Header.Set(staffWriteHeader, "")
	if s.staffMayReadOperatorChannels(req) {
		t.Fatal("an unconfigured staff credential accepted an empty header — empty must never match empty")
	}
}

// A channel id is not an authorization boundary (TKT-236 ai-review).
//
// The back office takes the id from a FORM FIELD, so a forged submit could name
// any channel in the database. Before this fix the UPDATE was scoped by
// (id, code) alone, and a caller holding another organizer's id and code could
// rename, re-kind, enable or disable their channel. Both are discoverable: the
// code is shown on the storefront selector, and the id leaks through any
// response that carries it.
//
// The refusal is 404, indistinguishable from a channel that does not exist. An
// answer that distinguished "not yours" from "no such thing" would confirm the
// id is real, which is the enumeration the scoping exists to prevent.
func TestUpdateChannelRefusesAnotherOrganizersChannel(t *testing.T) {
	e := newEnv(t)
	victim, attacker := uuid.New(), uuid.New()

	rec := channelWrite(e, http.MethodPost, "/channels", victim, createChannelBody("pos", "Their box office", "pos", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", rec.Code, rec.Body.String())
	}
	var theirs Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &theirs); err != nil {
		t.Fatal(err)
	}

	// The attacker knows the id AND the exact code — the strongest position a
	// forged form can be in — and still must not touch the row.
	rec = channelWrite(e, http.MethodPut, "/channels/"+theirs.Id.String(), attacker, map[string]any{
		"code": "pos", "display_name": "Hijacked", "kind": "web", "enabled": false,
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant update = %d, want 404 — a channel id is not an authorization "+
			"boundary, and the refusal must not distinguish 'not yours' from 'no such channel': %s",
			rec.Code, rec.Body.String())
	}

	// And nothing moved.
	rec = operatorListChannels(e, victim)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify read = %d", rec.Code)
	}
	var after ChannelList
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Channels) != 1 {
		t.Fatalf("victim has %d channels, want 1", len(after.Channels))
	}
	got := after.Channels[0]
	if got.DisplayName != "Their box office" || got.Kind != "pos" || !got.Enabled {
		t.Fatalf("the victim's channel was mutated: %+v", got)
	}
}

// The owner still updates their own channel — the scoping must be a boundary,
// not a wall.
func TestUpdateChannelAcceptsTheOwningOrganizer(t *testing.T) {
	e := newEnv(t)
	org := uuid.New()
	rec := channelWrite(e, http.MethodPost, "/channels", org, createChannelBody("pos", "Box office", "pos", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d", rec.Code)
	}
	var created Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	rec = channelWrite(e, http.MethodPut, "/channels/"+created.Id.String(), org, map[string]any{
		"code": "pos", "display_name": "Counter", "kind": "pos", "enabled": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner update = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// The enumeration→mutation chain, now PINNED AS CLOSED.
//
// ## What this test used to say, and why the history is kept
//
// Until TKT-245 this was `TestStaffCredentialCanStillEnumerateAndMutateAcross‑
// Tenants`, and it asserted the chain WORKED — a deliberate record of an open
// gap, in the shape of ADR-021's rollback-gap test. It existed because the
// comment above `staffMayReadOperatorChannels` twice claimed the opposite: pass 1
// of TKT-236's ai-review rejected "the added blast radius is nil"; pass 2
// rejected the replacement, "the organizer predicate breaks the chain". Both were
// written in good faith, both were false, and the second was settled only by
// running the sequence and watching it return 200.
//
// Its failure message named the condition for changing it: *"If this now REFUSES,
// the boundary has changed for the better — most likely TKT-245 landed an
// organizer identity catalog can verify... change this test to assert the
// refusal. Do not simply delete it: it is the record of what was open."* That is
// what happened, and this is that change.
//
// ## What it proves now
//
// The staff-write credential ALONE can no longer select a tenant: there is no
// query parameter to name one with, and the credential carries no organizer. An
// attacker holding it plus an assertion for their own organizer cannot reach
// another tenant's channel, because every write is scoped to the organizer the
// assertion names.
//
// ## What it still does NOT prove (ADR-021 — name the adversary)
//
// Nothing here constrains a holder of the SIGNING KEY, a compromised back office
// replaying a live assertion it legitimately holds, or anyone who can write
// catalog's database. ADR-058 states those residuals; this test is about a caller
// holding the deputy credential and nothing else.
//
// **If this test ever fails, the boundary changed and ADR-058 must be rewritten —
// do not delete it.**
func TestStaffCredentialAloneCanNoLongerEnumerateOrMutateAcrossTenants(t *testing.T) {
	e := newEnv(t)
	victim := uuid.New()

	// Seed the victim's channel by ACTING for them -- the only way a row can be
	// created for an organizer now.
	rec := channelWrite(e, http.MethodPost, "/channels", victim,
		createChannelBody("pos", "Victim box office", "pos", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", rec.Code, rec.Body.String())
	}
	var seeded Channel
	if err := json.Unmarshal(rec.Body.Bytes(), &seeded); err != nil {
		t.Fatal(err)
	}

	// Step 1 -- enumerate with the credential ALONE. There is no longer a query
	// parameter to name a victim with, and the credential carries no organizer, so
	// the request cannot express the tenant it wants.
	if rec := e.doWithHeaders(http.MethodGet, "/internal/channels", nil,
		map[string]string{staffWriteHeader: testStaffWriteToken}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("list with the credential alone = %d, want 401. The staff-write credential "+
			"authenticates the DEPUTY; on its own it must not select a tenant.\nbody: %s",
			rec.Code, rec.Body.String())
	}

	// Step 2 -- and an attacker holding the credential plus an assertion for their
	// OWN organizer cannot reach the victim's row: the update is scoped to the
	// organizer the assertion names, so the victim's channel id lands on no row.
	attacker := uuid.New()
	if rec := channelWrite(e, http.MethodPut, "/channels/"+seeded.Id.String(), attacker, map[string]any{
		"code": "pos", "display_name": "Renamed by a token holder", "kind": "web", "enabled": false,
	}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant update = %d, want 404.\n\nIf this SUCCEEDS again, the boundary "+
			"TKT-245 built has regressed and ADR-058's claim is false.\nbody: %s",
			rec.Code, rec.Body.String())
	}

	// And the victim's row is untouched -- a refusal that still wrote would look
	// identical from the status code alone.
	list := operatorListChannels(e, victim)
	var out ChannelList
	if err := json.Unmarshal(list.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Channels) != 1 || out.Channels[0].DisplayName != "Victim box office" {
		t.Fatalf("the victim's channel was modified: %+v", out.Channels)
	}
}
