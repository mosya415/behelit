package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A step ceiling is a proxy for "stop an agent grinding tokens with nothing to
// show", and as a proxy it cut long legitimate work short — in loop mode a turn
// spends a step per reply, so 50 is gone before a real task is half done. The
// word is how an operator removes the proxy now that the run has real bounds.
func TestAStepCeilingCanBeRemovedByName(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0}, // nobody said
		{"120", 120},
		{"unlimited", stepsUnlimited},
		{"none", stepsUnlimited},
		{"off", stepsUnlimited},
		{"UNLIMITED", stepsUnlimited},
		{"0", stepsUnlimited}, // what a person types when they mean "no cap"
	} {
		got, err := parseStepCeiling("steps", tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("parseStepCeiling(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"many", "-5", "1e3", "12 steps"} {
		if _, err := parseStepCeiling("steps", bad); err == nil {
			t.Fatalf("%q is not a step ceiling and must be refused", bad)
		}
	}
}

// -max-steps is a run ceiling over every role's own. Removing it has to be the
// same kind of statement: the operator saying "not this number", over a role
// that still carries its own 80.
func TestRemovedCeilingOutranksARolesOwn(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
	h := newHarness(t, fs.URL, "native", false)
	h.sess.agent.Steps = 80

	b := mustBudget(t, 45*time.Minute, stepsUnlimited, 0)
	h.orch.budget = b
	ctx, cancel := b.start(context.Background())
	defer cancel()
	_ = ctx
	if got := h.sess.maxSteps(); got != stepsUnlimited {
		t.Fatalf("the run's removed ceiling must outrank the role's 80, got %d", got)
	}

	// And the other direction still holds: a run ceiling BELOW the role's caps it.
	b2 := mustBudget(t, 45*time.Minute, 20, 0)
	h.orch.budget = b2
	ctx2, cancel2 := b2.start(context.Background())
	defer cancel2()
	_ = ctx2
	if got := h.sess.maxSteps(); got != 20 {
		t.Fatalf("a run ceiling of 20 must cap a role's 80, got %d", got)
	}
}

// Unattended AND unbounded in every dimension is the one combination nobody can
// afford, so `unlimited` needs a clock or a token ceiling beside it.
func TestUnlimitedStepsNeedARealBound(t *testing.T) {
	b, err := newRunBudget(nil, 0, stepsUnlimited, 0)
	if err != nil {
		t.Fatal(err)
	}
	if b.bounded() {
		t.Fatal("nothing but the step count bounds this run, so it is not bounded")
	}
	for _, with := range []*runBudget{
		mustBudget(t, 45*time.Minute, stepsUnlimited, 0),
		mustBudget(t, 0, stepsUnlimited, 4_000_000),
	} {
		if !with.bounded() {
			t.Fatalf("a clock or a token ceiling bounds a run: %s", with.describe())
		}
	}
}

func mustBudget(t *testing.T, d time.Duration, steps, tokens int) *runBudget {
	t.Helper()
	b, err := newRunBudget(nil, d, steps, tokens)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// roles.yaml speaks the same word, in both places that carry a step count.
func TestRolesFileRemovesTheCeiling(t *testing.T) {
	fs := newFakeServer(t, func(req fakeRequest, n int) fakeReply { return fakeReply{content: "x"} })
	h := newRoleHarness(t, fs, `
defaults:
  max_steps: unlimited
  max_tokens: 2000000
entry: lead
roles:
  lead:
    models: [m1]
    steps: unlimited
`, false)
	rc := h.orch.roles
	if rc.RunMaxSteps != stepsUnlimited {
		t.Fatalf("defaults: max_steps: unlimited → %d", rc.RunMaxSteps)
	}
	var lead *Agent
	for _, a := range rc.Roles {
		if a.Name == "lead" {
			lead = a
		}
	}
	if lead == nil || lead.Steps != stepsUnlimited {
		t.Fatalf("a role's own steps: unlimited was not read: %+v", lead)
	}
	if !strings.Contains(rc.YAML(), "max_steps: unlimited") {
		t.Fatalf("what lca reports back has to say the same thing:\n%s", rc.YAML())
	}
}
