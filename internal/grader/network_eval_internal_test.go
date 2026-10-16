package grader

import "testing"

// TestOutcomeZeroValueIsUnknown: an outcome nobody judged must never read as
// "holds", or a forgotten assignment would PASS a network criterion.
func TestOutcomeZeroValueIsUnknown(t *testing.T) {
	var o netOutcome
	if o.state != stateUnknown {
		t.Fatalf("zero outcome state = %v, want unknown", o.state)
	}
	if got := o.evidence()["state"]; got != "unknown" {
		t.Fatalf("zero outcome evidence state = %v, want unknown", got)
	}
	// A deny expectation without any pod check proves nothing.
	c := &NetworkChecker{}
	empty := &netOutcome{exp: netExpect{name: "deny-other"}}
	c.judgeDenyOutcome(empty, nil, nil, destination{})
	if empty.state != stateUnknown {
		t.Fatalf("a deny with no pod checks = %v, want unknown", empty.state)
	}
}
