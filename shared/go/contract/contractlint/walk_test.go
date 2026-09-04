package contractlint

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmittedRetainsCollidingReceiverMethods(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "collisions.go"), `package api
import "net/http"
type qrLinkSigner struct{}
type feedCursorSigner struct{}
type priceResolution struct{}
type feeResolution struct{}
func (qrLinkSigner) sign() { writeJSON(nil, http.StatusUnauthorized, nil) }
func (feedCursorSigner) sign() { writeJSON(nil, http.StatusServiceUnavailable, nil) }
func (priceResolution) validate() { writeJSON(nil, http.StatusConflict, nil) }
func (feeResolution) validate() { writeJSON(nil, http.StatusInternalServerError, nil) }
func accessHandler() { qrLinkSigner{}.sign() }
func commerceHandler() { priceResolution{}.validate() }
func writeJSON(any, int, any) {}
`)
	pkg, err := ParsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{WriteFuncs: []string{"writeJSON"}, StatusArg: 1}
	for _, test := range []struct {
		name string
		root string
		want []int
	}{
		{name: "sign", root: "accessHandler", want: []int{http.StatusUnauthorized, http.StatusServiceUnavailable}},
		{name: "validate", root: "commerceHandler", want: []int{http.StatusConflict, http.StatusInternalServerError}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := pkg.Emitted([]string{test.root}, cfg)
			for _, status := range test.want {
				if !containsStatus(got, status) {
					t.Fatalf("Emitted(%q) = %v, want status %d from every colliding %s method", test.root, got, status, test.name)
				}
			}
		})
	}
}

func TestEmittedUsesConfiguredReturnedStatusesOnlyAtWrites(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "returned.go"), `package api
import "net/http"
func direct() { writeJSON(nil, terminalCode("timeout"), nil) }
func assigned() {
	code, message := problem(nil)
	writeJSON(nil, code, message)
}
func unrelated() {
	_ = terminalCode("timeout")
	writeJSON(nil, http.StatusOK, nil)
}
func terminalCode(status string) int {
	if status == "timeout" { return http.StatusRequestTimeout }
	return http.StatusPaymentRequired
}
func problem(err error) (int, string) {
	if err != nil { return http.StatusConflict, "conflict" }
	return http.StatusInternalServerError, "failed"
}
func writeJSON(any, int, any) {}
`)
	pkg, err := ParsePackage(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		WriteFuncs: []string{"writeJSON"},
		StatusArg:  1,
		ReturnedStatuses: map[string][]int{
			"terminalCode": {http.StatusPaymentRequired, http.StatusRequestTimeout},
			"problem":      {http.StatusConflict, http.StatusInternalServerError},
		},
	}
	if err := pkg.validateReturnedStatuses(cfg); err != nil {
		t.Fatalf("validateReturnedStatuses() error = %v", err)
	}
	withoutProblem := cfg
	withoutProblem.ReturnedStatuses = map[string][]int{
		"terminalCode": {http.StatusPaymentRequired, http.StatusRequestTimeout},
	}
	if err := pkg.validateReturnedStatuses(withoutProblem); err == nil ||
		!strings.Contains(err.Error(), "problem") {
		t.Fatalf("validateReturnedStatuses() error = %v, want missing problem helper", err)
	}
	for _, test := range []struct {
		root     string
		want     []int
		unwanted []int
	}{
		{
			root: "direct",
			want: []int{http.StatusPaymentRequired, http.StatusRequestTimeout},
		},
		{
			root: "assigned",
			want: []int{http.StatusConflict, http.StatusInternalServerError},
		},
		{
			root:     "unrelated",
			want:     []int{http.StatusOK},
			unwanted: []int{http.StatusPaymentRequired, http.StatusRequestTimeout},
		},
	} {
		t.Run(test.root, func(t *testing.T) {
			got := pkg.Emitted([]string{test.root}, cfg)
			for _, status := range test.want {
				if !containsStatus(got, status) {
					t.Fatalf("Emitted(%q) = %v, want status %d", test.root, got, status)
				}
			}
			for _, status := range test.unwanted {
				if containsStatus(got, status) {
					t.Fatalf("Emitted(%q) = %v, did not want unrelated status %d", test.root, got, status)
				}
			}
		})
	}
}
