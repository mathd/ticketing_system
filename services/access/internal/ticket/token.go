package ticket

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Claims struct {
	Version     int       `json:"v"`
	TicketID    uuid.UUID `json:"tid"`
	OrderID     uuid.UUID `json:"oid"`
	OrganizerID uuid.UUID `json:"org"`
	SlotID      uuid.UUID `json:"sid"`
	IssuedAt    int64     `json:"iat"`
}

type Signer struct {
	private ed25519.PrivateKey
	kid     string
}

func New(seedBase64, kid string) (*Signer, error) {
	seed, err := base64.RawStdEncoding.DecodeString(seedBase64)
	if err != nil {
		return nil, fmt.Errorf("decode ACCESS_QR_PRIVATE_KEY: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("ACCESS_QR_PRIVATE_KEY must be an Ed25519 seed")
	}
	if kid == "" {
		return nil, fmt.Errorf("ACCESS_QR_KID required")
	}
	return &Signer{private: ed25519.NewKeyFromSeed(seed), kid: kid}, nil
}

func (s *Signer) Sign(c Claims) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "kid": s.kid, "typ": "TKT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(s.private, []byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *Signer) Payload(ticketID, orderID, organizerID, slotID uuid.UUID, issuedAt time.Time) (string, error) {
	return s.Sign(Claims{Version: 1, TicketID: ticketID, OrderID: orderID, OrganizerID: organizerID, SlotID: slotID, IssuedAt: issuedAt.UTC().Unix()})
}

func Verify(token string, keys map[string]ed25519.PublicKey) (Claims, error) {
	var zero Claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return zero, fmt.Errorf("invalid token")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return zero, fmt.Errorf("decode header: %w", err)
	}
	var header struct{ Alg, Kid string }
	if err = json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "EdDSA" || header.Kid == "" {
		return zero, fmt.Errorf("invalid header")
	}
	key := keys[header.Kid]
	if len(key) != ed25519.PublicKeySize {
		return zero, fmt.Errorf("unknown key")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return zero, fmt.Errorf("invalid signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return zero, err
	}
	if err = json.Unmarshal(payload, &zero); err != nil {
		return zero, err
	}
	if zero.Version != 1 || zero.TicketID == uuid.Nil || zero.OrderID == uuid.Nil || zero.OrganizerID == uuid.Nil || zero.SlotID == uuid.Nil {
		return Claims{}, fmt.Errorf("invalid claims")
	}
	return zero, nil
}
