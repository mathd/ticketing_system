// Package delivery carries the one act both the issuance consumer and the staff
// redelivery route perform: resolve a buyer's address from commerce, hand the
// capability link to the transport, and record that it was accepted.
//
// It exists because the two callers must not drift. The consumer's `deliver` and a
// resend build the SAME link from the SAME guest reference and resolve the SAME
// address from the SAME commerce route — two copies would be two places to get the
// capability URL's shape wrong, and only one of them is exercised by a checkout.
package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Mailer is the transport. Structurally identical to the consumer's Mailer, and
// deliberately declared here rather than imported from it: the dependency runs this
// way (the consumer may use this package; this package must not reach back into the
// consumer) and Go's structural typing means one implementation satisfies both.
//
// WHAT THIS PORT DOES NOT DO, stated so no caller claims otherwise: the only
// implementation in this repo is the consumer's LogMailer, which hashes the recipient
// and the link, logs them, and returns nil. This platform has had a mail PORT since
// issuance shipped and has never had a SENDER (ADR-050, shared/go/mail). So "the
// transport accepted the message" is the strongest claim any caller may make; "the
// customer received an email" is not available and must not be written down.
type Mailer interface {
	Send(ctx context.Context, messageID uuid.UUID, email, link string) error
}

// AddressBook resolves a buyer's delivery address. Commerce owns buyer PII (ADR-012);
// access reads it at send time and never persists or logs it.
type AddressBook interface {
	DeliveryEmail(ctx context.Context, buyerID uuid.UUID) (string, error)
}

// CommerceAddressBook reads commerce's internal delivery-email route with the shared
// service token, which access already holds and already uses for exactly this.
type CommerceAddressBook struct {
	Client  *http.Client
	BaseURL string
	Token   string
}

func (a CommerceAddressBook) DeliveryEmail(ctx context.Context, buyerID uuid.UUID) (string, error) {
	url := strings.TrimSuffix(a.BaseURL, "/") + "/internal/buyers/" + buyerID.String() + "/delivery-email"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Internal-Token", a.Token)
	res, err := a.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		// The status, never the body: an error body from commerce could carry the
		// address or the buyer id, and this error is on its way to a log line.
		return "", fmt.Errorf("commerce delivery address: %d", res.StatusCode)
	}
	var v struct {
		Email string `json:"email"`
	}
	if err = json.NewDecoder(res.Body).Decode(&v); err != nil || v.Email == "" {
		return "", fmt.Errorf("invalid commerce delivery address")
	}
	return v.Email, nil
}

// TicketLink builds the guest retrieval URL for one order reference.
//
// ONE definition, used by issuance and by resend. The value in it is a live bearer
// capability (ADR-012): anyone holding this URL can retrieve the tickets, nothing
// revokes it, and every send widens the set of holders. It must not be logged, echoed
// in a response, or rendered on a staff console.
func TicketLink(publicURL string, ref uuid.UUID) string {
	return strings.TrimSuffix(publicURL, "/") + "/en/tickets/" + ref.String()
}
