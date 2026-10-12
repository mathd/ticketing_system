// Package catalog is Access's read of Catalog's performance data (TKT-365, ADR-083).
//
// The ticket bundle shows history times in the zone of each ticket's performance, and
// Catalog owns that zone. Access holds only the slot id. This client asks Catalog's
// internal display resolver for the zones of a set of slot ids.
//
// Every failure is an ABSENT zone, never an error. The bundle is the page a buyer opens
// at the gate, and a time zone is presentation: an unknown zone shows as labelled UTC,
// while a refused bundle shows nothing at all.
package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxIDsPerRequest is the resolver's contract bound on `ids` (catalog openapi,
// /internal/performances/display-names).
const MaxIDsPerRequest = 20

// DefaultTimeout bounds ALL the requests of one PerformanceZones call. It stays well
// under the storefront's 5 s upstream deadline, so a slow Catalog delays the bundle but
// never makes the storefront give up on it (ADR-083).
const DefaultTimeout = 1500 * time.Millisecond

// resolverLocale is a fixed supported locale. The resolver requires one because it also
// resolves the event name, which Access does not read.
const resolverLocale = "en"

// Client reads performance zones from Catalog with the shared internal service token.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
	// Timeout overrides DefaultTimeout. Zero means DefaultTimeout.
	Timeout time.Duration
	// Log receives one warning per failed request. Nil means slog.Default().
	Log *slog.Logger
}

// PerformanceZones returns the zone of each id Catalog knows, keyed by id. An id that is
// unknown, or whose request failed, is absent from the map.
func (c Client) PerformanceZones(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	unique := make([]uuid.UUID, 0, len(ids))
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return out
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for start := 0; start < len(unique); start += MaxIDsPerRequest {
		chunk := unique[start:min(start+MaxIDsPerRequest, len(unique))]
		zones, err := c.fetch(ctx, chunk)
		if err != nil {
			c.logger().WarnContext(ctx, "performance zones unavailable; tickets will show UTC times",
				"ids", len(chunk), "err", err)
			continue
		}
		// Map by id, and only ids this request asked for: a zone is never taken from a
		// position in the response or from an entry nobody requested.
		for _, id := range chunk {
			if zone, ok := zones[id]; ok {
				out[id] = zone
			}
		}
	}
	return out
}

func (c Client) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

type statusError int

func (e statusError) Error() string {
	return "catalog display resolver answered " + http.StatusText(int(e))
}

func (c Client) fetch(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.String()
	}
	query := url.Values{"locale": {resolverLocale}, "ids": {strings.Join(raw, ",")}}
	endpoint := strings.TrimSuffix(c.BaseURL, "/") + "/internal/performances/display-names?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Internal-Token", c.Token)
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, statusError(res.StatusCode)
	}
	var body struct {
		Performances []struct {
			PerformanceID uuid.UUID `json:"performance_id"`
			Timezone      string    `json:"timezone"`
		} `json:"performances"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	zones := make(map[uuid.UUID]string, len(body.Performances))
	for _, p := range body.Performances {
		if p.Timezone != "" {
			zones[p.PerformanceID] = p.Timezone
		}
	}
	return zones, nil
}
