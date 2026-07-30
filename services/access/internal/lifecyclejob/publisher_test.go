package lifecyclejob

import (
	"context"
	"strings"
	"testing"
)

// Access boot-checks THREE alarm durables through this one helper, each with its own
// environment variable. Every message used to name the integrity class, so an operator who
// forgot ACCESS_ADMISSION_CONFLICT_DURABLE was told to set ACCESS_LIFECYCLE_ALARM_DURABLE —
// a variable that was already set. Only the caller's wrap disambiguated, and it sits
// outside the sentence that names the fix (TKT-119).
//
// The assertions are deliberately exact: these strings ARE the operator remediation
// contract, and a test that only checked "an error was returned" would have passed against
// the defect. Each case also asserts the absence of "integrity" for the two classes that
// are not it — without that, every message the old code produced would still satisfy this.
func TestRequireAlarmRouteNamesItsOwnClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		route    AlarmRoute
		wantEnv  string
		wantWord string
	}{
		{
			name:     "integrity",
			route:    AlarmRoute{Stream: "PLATFORM", Subject: "platform.access.lifecycle-integrity.alarm", DurableEnv: "ACCESS_LIFECYCLE_ALARM_DURABLE", Class: "integrity"},
			wantEnv:  "ACCESS_LIFECYCLE_ALARM_DURABLE",
			wantWord: "integrity",
		},
		{
			name:     "admission conflict",
			route:    AlarmRoute{Stream: "PLATFORM", Subject: "platform.access.admission-conflict.alarm", DurableEnv: "ACCESS_ADMISSION_CONFLICT_DURABLE", Class: "admission conflict"},
			wantEnv:  "ACCESS_ADMISSION_CONFLICT_DURABLE",
			wantWord: "admission conflict",
		},
		{
			name:     "policy conflict",
			route:    AlarmRoute{Stream: "PLATFORM", Subject: "platform.access.admission-policy-conflict.alarm", DurableEnv: "ACCESS_POLICY_CONFLICT_DURABLE", Class: "policy conflict",
			},
			wantEnv:  "ACCESS_POLICY_CONFLICT_DURABLE",
			wantWord: "policy conflict",
		},
	} {
		t.Run(tc.name+" missing durable", func(t *testing.T) {
			// Durable is empty, so this returns before touching JetStream: no broker, no
			// fake, and it is exactly the message the ticket is about.
			err := RequireAlarmRoute(context.Background(), nil, tc.route)
			if err == nil {
				t.Fatal("a missing durable must refuse boot: fail-open needs somewhere for alarms to land")
			}
			if !strings.Contains(err.Error(), tc.wantEnv) {
				t.Fatalf("error must name the variable the operator has to set (%s): %v", tc.wantEnv, err)
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Fatalf("error must name the %q class: %v", tc.wantWord, err)
			}
			// The defect, stated as an assertion: a non-integrity class must not be
			// described as the integrity one.
			if tc.wantWord != "integrity" && strings.Contains(err.Error(), "integrity") {
				t.Fatalf("the %s class is described as integrity: %v", tc.wantWord, err)
			}
			// And it must never name a variable that is not this class's.
			for _, other := range []string{"ACCESS_LIFECYCLE_ALARM_DURABLE", "ACCESS_ADMISSION_CONFLICT_DURABLE", "ACCESS_POLICY_CONFLICT_DURABLE"} {
				if other != tc.wantEnv && strings.Contains(err.Error(), other) {
					t.Fatalf("error names %s, which is not this class's variable: %v", other, err)
				}
			}
		})
	}
}
