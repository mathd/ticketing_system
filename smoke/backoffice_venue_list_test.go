//go:build smoke

package smoke_test

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// US-018 / TKT-101, amended by TKT-190 (US-B1). The back-office venue list, end
// to end through the real stack — now behind a staff session.
//
// This file is the closest thing in the repo to a browser: it drives the real
// gateway, the real Astro SSR layer, the real catalog and real Postgres, and it
// SUBMITS forms rather than only rendering them. What it still cannot prove is
// browser-side behaviour — that a browser actually sends Origin on a same-origin
// POST, and that it honours SameSite. Those two are covered by the manual
// browser check in the ticket's DoD, and only there (there is no Playwright
// harness in-repo; docs/learnings/2026-07-20-browser-submit-is-the-only-checkorigin-catch.md).
const seededVenueName = "La Grande Salle" // migrations/0008_seed_default_venues.sql

// gatewayOrigin is the public origin a browser addressing this stack would send.
// The middleware compares Origin against the origin the gateway reports through
// X-Forwarded-Proto/Host, so a submission that omits it is refused — which is
// itself asserted below.
var gatewayOrigin = strings.TrimSuffix(gatewayURL, "/")

func TestBackofficeVenueReadHoursTier(t *testing.T) {
	// One fetch: header and body asserted on the SAME response (getWithHeaders
	// carries a 10s timeout and runs the contract check).
	code, body, hdr := getWithHeaders(t,
		gatewayURL+"/api/catalog/public/venues?organizer_id="+organizerID)
	if code != http.StatusOK {
		t.Fatalf("venue read: status %d: %s", code, body)
	}
	// ADR-004 hours tier — long-lived venue geometry, not the events minutes tier.
	if got := hdr.Get("Cache-Control"); got != "public, max-age=3600, s-maxage=3600" {
		t.Fatalf("venue read must carry the ADR-004 hours tier, got %q", got)
	}
	if !strings.Contains(string(body), seededVenueName) {
		t.Fatalf("venue read must list the seeded venue %q; body=%s", seededVenueName, body)
	}
}

// COS-1. Every path under /admin/ is gated except the three anonymous ones, and
// an unknown path is gated identically to a real one — otherwise the difference
// between "redirect" and "404" would map the admin surface for anyone who asked.
//
// The assertion is on where the caller ENDS UP, not on a single status code.
// Astro normalizes a missing trailing slash with its own 307 before any
// middleware runs, so `/admin` answers 307 -> `/admin/` -> 302 -> login. Demanding
// an immediate 302 would fail that path while the chain is perfectly safe, and
// what COS-1 actually promises is that the venue list never renders. So: follow
// the chain, assert it terminates at the login page, and assert no response along
// the way carried venue data.
func TestBackofficeRefusesAnonymousCallers(t *testing.T) {
	for _, path := range []string{"/admin/", "/admin", "/admin/venues/whatever", "/admin/definitely-not-a-page"} {
		target := gatewayURL + path
		var landed string
		for hop := 0; ; hop++ {
			if hop > 5 {
				t.Fatalf("GET %s anonymously: redirect loop, last target %s", path, target)
			}
			resp := doRequest(t, noRedirectClient(), http.MethodGet, target, nil, nil)
			body := readBody(t, resp)
			if strings.Contains(body, seededVenueName) {
				t.Fatalf("GET %s anonymously leaked venue data at %s: %.300s", path, target, body)
			}
			loc := resp.Header.Get("Location")
			if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
				if resp.StatusCode == http.StatusOK {
					t.Fatalf("GET %s anonymously terminated with 200 at %s — nothing under /admin/ "+
						"may render without a session; body=%.300s", path, target, body)
				}
				t.Fatalf("GET %s anonymously terminated at %s with status %d, want the login page",
					path, target, resp.StatusCode)
			}
			landed = loc
			if strings.HasSuffix(loc, "/admin/login") {
				break
			}
			if strings.HasPrefix(loc, "http") {
				target = loc
			} else {
				target = gatewayURL + loc
			}
		}
		if !strings.HasSuffix(landed, "/admin/login") {
			t.Fatalf("GET %s anonymously landed at %q, want /admin/login", path, landed)
		}
	}
}

// The health probe MUST stay anonymous: Compose hits it directly on the
// container, so gating it makes the back office unhealthy, which makes the
// gateway's depends_on never satisfy and the whole stack fail to start.
func TestBackofficeHealthzStaysAnonymous(t *testing.T) {
	resp := doRequest(t, noRedirectClient(), http.MethodGet, gatewayURL+"/admin/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/healthz must stay reachable without a session, got %d: %.200s",
			resp.StatusCode, readBody(t, resp))
	}
}

// COS-2 and COS-3 in one session, because they are one story: sign in through
// the real form, reload, sign out, then replay the captured cookie.
func TestBackofficeSignInReloadAndSignOut(t *testing.T) {
	identifier, password := staffCredential(t)
	client := jarClient(t)

	// The login page is anonymous, and its form action is relative — an absolute
	// one is the second trap TKT-105 paid for.
	page := readBody(t, doRequest(t, client, http.MethodGet, gatewayURL+"/admin/login", nil, nil))
	for _, want := range []string{`name="identifier"`, `name="password"`, `method="post"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("login page is missing %s; page=%.600s", want, page)
		}
	}
	if strings.Contains(page, `action="http`) {
		t.Fatalf("the login form action must be relative, not absolute; page=%.600s", page)
	}

	// Submit it the way a browser would: urlencoded, with an Origin header.
	resp := postForm(t, client, gatewayURL+"/admin/login", url.Values{
		"identifier": {identifier}, "password": {password}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: status %d, want 303; body=%.300s", resp.StatusCode, readBody(t, resp))
	}
	session := sessionCookie(t, resp)

	// COS-2: the venue list renders, and it survives a reload on the same cookie.
	for _, attempt := range []string{"first", "after reload"} {
		resp := doRequest(t, client, http.MethodGet, gatewayURL+"/admin/", nil, nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, seededVenueName) {
			t.Fatalf("%s authenticated venue list: status %d, body=%.400s", attempt, resp.StatusCode, body)
		}
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("%s: authenticated pages must be no-store, got %q", attempt, cc)
		}
	}

	// COS-3: sign out, then REPLAY the captured cookie from a client with no jar.
	// Checking that the browser was told to drop the cookie proves nothing — the
	// value is what an attacker keeps.
	if resp := postForm(t, client, gatewayURL+"/admin/logout", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-out: status %d, want 303; body=%.300s", resp.StatusCode, readBody(t, resp))
	}
	replay := doRequest(t, noRedirectClient(), http.MethodGet, gatewayURL+"/admin/",
		map[string]string{"Cookie": session}, nil)
	if replay.StatusCode != http.StatusFound {
		t.Fatalf("replaying the signed-out cookie: status %d, want 302 — the session was not "+
			"invalidated server-side; body=%.300s", replay.StatusCode, readBody(t, replay))
	}
}

// COS-4 at the wire: an unknown identifier and a wrong password are one answer.
func TestBackofficeRefusesBadCredentialsIdentically(t *testing.T) {
	identifier, _ := staffCredential(t)

	unknown := readBody(t, postForm(t, jarClient(t), gatewayURL+"/admin/login", url.Values{
		"identifier": {"nobody-at-all@example.test"}, "password": {"whatever"}}))
	wrong := readBody(t, postForm(t, jarClient(t), gatewayURL+"/admin/login", url.Values{
		"identifier": {identifier}, "password": {"definitely not the password"}}))

	if unknown != wrong {
		t.Fatalf("the two refusals differ and so disclose which accounts exist:\n unknown=%.400s\n wrong=%.400s",
			unknown, wrong)
	}
	// Echoing the identifier back would put probed addresses into anything that
	// captures rendered output, and makes the page a reflection surface.
	for _, leak := range []string{identifier, "nobody-at-all@example.test"} {
		if strings.Contains(unknown, leak) {
			t.Fatalf("the refusal echoes the submitted identifier %q: %.400s", leak, unknown)
		}
	}
}

// COS-7, the server-side half. checkOrigin is off (it cannot work behind the
// proxy), so this is what stands in its place: a submission whose Origin is not
// the gateway's public origin is refused BEFORE any credential is read.
//
// ai-review F5: "the response was 403" is not that claim. A middleware that
// authenticated first, created the session, and only then rewrote the response
// to 403 would satisfy it exactly. So each case asserts the STATE the request
// was supposed to be refused before reaching:
//   - a cross-origin login runs on a FRESH, unauthenticated jar, and must emit no
//     session cookie and leave that jar still signed out;
//   - a cross-origin logout runs on a live session, which must survive it.
func TestBackofficeRefusesCrossOriginSubmissions(t *testing.T) {
	identifier, password := staffCredential(t)
	creds := url.Values{"identifier": {identifier}, "password": {password}}

	crossOriginPost := func(t *testing.T, c *http.Client, path string, origin string) *http.Response {
		t.Helper()
		hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		if origin != "" {
			hdr["Origin"] = origin
		}
		return doRequest(t, c, http.MethodPost, gatewayURL+path, hdr, strings.NewReader(creds.Encode()))
	}

	// Two login attempts with VALID credentials — the only kind that could
	// actually mint a session if the ordering were wrong.
	for _, tc := range []struct{ name, origin string }{
		{"cross-site login", "http://evil.example"},
		{"login with no Origin at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := jarClient(t) // unauthenticated, so a minted session has nowhere to hide
			resp := crossOriginPost(t, fresh, "/admin/login", tc.origin)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status %d, want 403; body=%.300s", resp.StatusCode, readBody(t, resp))
			}
			for _, c := range resp.Cookies() {
				if c.Name == "bo_sid" && c.Value != "" {
					t.Fatalf("the refused submission still minted a session cookie — the origin " +
						"check ran AFTER the credentials were verified")
				}
			}
			// And the jar really is still signed out: valid credentials were
			// submitted, so anything short of a redirect to login means a session
			// was created somewhere.
			after := doRequest(t, fresh, http.MethodGet, gatewayURL+"/admin/", nil, nil)
			if after.StatusCode != http.StatusFound {
				t.Fatalf("after a refused cross-origin sign-in the caller is not anonymous: status %d",
					after.StatusCode)
			}
		})
	}

	// A live session, to prove a forged logout cannot end it.
	client := jarClient(t)
	if resp := postForm(t, client, gatewayURL+"/admin/login", creds); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup sign-in failed: %d", resp.StatusCode)
	}
	if resp := crossOriginPost(t, client, "/admin/logout", "http://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site logout: status %d, want 403; body=%.300s", resp.StatusCode, readBody(t, resp))
	}
	// A 403 that still destroyed the session would be a refusal in name only.
	if resp := doRequest(t, client, http.MethodGet, gatewayURL+"/admin/", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the refused cross-site logout still ended the session: status %d", resp.StatusCode)
	}
}

// --- helpers ---

func staffCredential(t *testing.T) (identifier, password string) {
	t.Helper()
	identifier, password = os.Getenv("SMOKE_STAFF_IDENTIFIER"), os.Getenv("SMOKE_STAFF_PASSWORD")
	if identifier == "" || password == "" {
		t.Skip("SMOKE_STAFF_IDENTIFIER/SMOKE_STAFF_PASSWORD not set (scripts/smoke.sh provisions them)")
	}
	return identifier, password
}

// noRedirectClient returns the redirect itself rather than following it: the
// status and Location ARE the assertion for a gated path.
func noRedirectClient() *http.Client {
	return &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// jarClient keeps cookies across calls, the way a browser does, so a sign-in and
// the reads after it are one session.
func jarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	c := noRedirectClient()
	c.Jar = jar
	return c
}

func doRequest(t *testing.T, c *http.Client, method, target string, hdr map[string]string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, target, err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// postForm submits urlencoded fields with the gateway's public Origin — what a
// browser on this stack sends.
func postForm(t *testing.T, c *http.Client, target string, form url.Values) *http.Response {
	t.Helper()
	return doRequest(t, c, http.MethodPost, target, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       gatewayOrigin,
	}, strings.NewReader(form.Encode()))
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// sessionCookie extracts the raw Cookie header value to replay after sign-out.
func sessionCookie(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name != "bo_sid" || c.Value == "" {
			continue
		}
		// COS-6, asserted on the wire rather than only in a unit test.
		if !c.HttpOnly {
			t.Fatal("the session cookie must be HttpOnly")
		}
		if c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("the session cookie must be SameSite=Lax, got %v", c.SameSite)
		}
		// Scoped to the back office, not the whole origin (ai-review F3): the
		// gateway serves the storefront, the scanner and /api/* on this SAME
		// origin, and a Path=/ cookie is handed to every one of them.
		if c.Path != "/admin" {
			t.Fatalf("the session cookie must be scoped to /admin, got %q", c.Path)
		}
		if c.MaxAge <= 0 || c.MaxAge > 24*60*60 {
			t.Fatalf("the session cookie lifetime must be bounded and under a day, got %ds", c.MaxAge)
		}
		return c.Name + "=" + c.Value
	}
	t.Fatalf("sign-in set no session cookie: %v", resp.Cookies())
	return ""
}

// TKT-197 COS-2/4/5, through the real stack: the role matrix refuses, and
// hiding a link is not what does it.
//
// The seeded venue is deterministic (migration 0008), so the authoring URL is
// known without an admin first creating one — which matters, because the point
// is to hand a NON-admin a URL they were never shown.
const seededVenueID = "00000000-0000-0000-0000-0000000000a1"

func roleCredential(t *testing.T, role string) (identifier, password string) {
	t.Helper()
	var idEnv, pwEnv string
	switch role {
	case "admin":
		idEnv, pwEnv = "SMOKE_STAFF_IDENTIFIER", "SMOKE_STAFF_PASSWORD"
	case "box_office":
		idEnv, pwEnv = "SMOKE_BOXOFFICE_IDENTIFIER", "SMOKE_BOXOFFICE_PASSWORD"
	case "finance":
		idEnv, pwEnv = "SMOKE_FINANCE_IDENTIFIER", "SMOKE_FINANCE_PASSWORD"
	default:
		t.Fatalf("no credential wired for role %q", role)
	}
	identifier, password = os.Getenv(idEnv), os.Getenv(pwEnv)
	if identifier == "" || password == "" {
		t.Skipf("%s/%s not set (scripts/smoke.sh provisions them)", idEnv, pwEnv)
	}
	return identifier, password
}

func signInAs(t *testing.T, role string) *http.Client {
	t.Helper()
	identifier, password := roleCredential(t, role)
	client := jarClient(t)
	resp := postForm(t, client, gatewayURL+"/admin/login", url.Values{
		"identifier": {identifier}, "password": {password}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in as %s: status %d; body=%.300s", role, resp.StatusCode, readBody(t, resp))
	}
	return client
}

func TestBackofficeRoleMatrixRefusesTheAuthoringSurface(t *testing.T) {
	authoring := gatewayURL + "/admin/venues/" + seededVenueID

	// admin reaches it — without this the refusals below could be a broken URL.
	adminResp := doRequest(t, signInAs(t, "admin"), http.MethodGet, authoring, nil, nil)
	if adminResp.StatusCode != http.StatusOK {
		t.Fatalf("admin must reach the authoring surface: status %d; body=%.300s",
			adminResp.StatusCode, readBody(t, adminResp))
	}

	for _, role := range []string{"box_office", "finance"} {
		t.Run(role, func(t *testing.T) {
			client := signInAs(t, role)

			// COS-5, first half: the link is not rendered.
			list := readBody(t, doRequest(t, client, http.MethodGet, gatewayURL+"/admin/", nil, nil))
			if strings.Contains(list, "/admin/venues/") {
				t.Fatalf("the venue list offers %s an authoring link it may not use: %.400s", role, list)
			}

			// COS-5, second half — the one that matters. Paste the URL anyway.
			// A hidden link is a courtesy; if this returns 200 the whole matrix is
			// decoration.
			resp := doRequest(t, client, http.MethodGet, authoring, nil, nil)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s reached the authoring surface by URL: status %d — hiding the link is "+
					"not the enforcement; body=%.300s", role, resp.StatusCode, readBody(t, resp))
			}

			// COS-4: the refusal must not distinguish a real route from an
			// imaginary one, or probing maps the admin surface.
			imaginary := doRequest(t, client, http.MethodGet, gatewayURL+"/admin/settlement", nil, nil)
			if imaginary.StatusCode != resp.StatusCode {
				t.Fatalf("%s: real route %d vs imaginary route %d — the difference tells them what exists",
					role, resp.StatusCode, imaginary.StatusCode)
			}
			body := readBody(t, resp)
			for _, leak := range []string{"admin", "role", "venues"} {
				if strings.Contains(strings.ToLower(body), leak) {
					t.Fatalf("%s: the refusal names %q: %s", role, leak, body)
				}
			}
		})
	}
}

// Every role must still be able to reach the venue list and sign out — a matrix
// that locks staff out of their own session would be a different bug.
func TestBackofficeRoleMatrixAdmitsSharedRoutes(t *testing.T) {
	for _, role := range []string{"admin", "box_office", "finance"} {
		t.Run(role, func(t *testing.T) {
			client := signInAs(t, role)
			resp := doRequest(t, client, http.MethodGet, gatewayURL+"/admin/", nil, nil)
			if resp.StatusCode != http.StatusOK || !strings.Contains(readBody(t, resp), seededVenueName) {
				t.Fatalf("%s cannot see the venue list: status %d", role, resp.StatusCode)
			}
			if out := postForm(t, client, gatewayURL+"/admin/logout", nil); out.StatusCode != http.StatusSeeOther {
				t.Fatalf("%s cannot sign out: status %d", role, out.StatusCode)
			}
		})
	}
}
