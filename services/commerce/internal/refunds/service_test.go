package refunds

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	commercestore "ticketing/services/commerce/internal/store"
)

func TestRefundPaymentClassifiesProviderTerminalAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		want error
	}{
		{"valid terminal", http.StatusUnprocessableEntity, `{"code":"provider_refund_failed","provider_ref":"re_terminal"}`, ErrProviderRefundFailed},
		{"wrong code", http.StatusUnprocessableEntity, `{"code":"other","provider_ref":"re_terminal"}`, ErrPaymentsUnresolved},
		{"missing reference", http.StatusUnprocessableEntity, `{"code":"provider_refund_failed"}`, ErrPaymentsUnresolved},
		{"malformed body", http.StatusUnprocessableEntity, `{`, ErrPaymentsUnresolved},
		{"gateway failure", http.StatusBadGateway, `{}`, ErrPaymentsUnresolved},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &Service{paymentsURL: "http://payments", call: func(_ context.Context, method, url, _ string, _ any, _ bool) (int, []byte, error) {
				if method != http.MethodPost || !strings.HasSuffix(url, "/internal/psp/partial-refund") {
					t.Fatalf("unexpected payment call %s %s", method, url)
				}
				return tc.code, []byte(tc.body), nil
			}}
			_, err := service.refundPayment(context.Background(), commercestore.Refund{
				ID: uuid.New(), OrganizerID: uuid.New(), PaymentSourceKey: "checkout", Amount: 500, Currency: "CAD",
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("refundPayment error = %v, want %v", err, tc.want)
			}
		})
	}
}
