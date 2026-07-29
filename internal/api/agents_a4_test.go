package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FanDoster/Build-System/internal/agents"
)

// A4 lives in its own file: agents_test.go is large and this keeps the
// transport and clock work separable from the protocol tests around it.

// The container listens in plaintext on loopback behind nginx, so r.TLS is nil
// for every request this server will ever serve. Guessing "http" when the
// header is absent would accuse every agent in the fleet of having leaked its
// token the moment one nginx directive went missing.
func TestRequestSchemeSaysUnknownRatherThanAccusing(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"", ""},                 // nothing in front, or a regression: we do not know
		{"https", "https"},       // the ordinary production case
		{"http", "http"},         // a genuine plaintext hop, worth shouting about
		{"HTTPS", "https"},       // some proxies capitalise
		{" https ", "https"},     // and pad
		{"https, http", "https"}, // chained proxies append; the first hop is ours
	} {
		r := httptest.NewRequest("POST", "/api/agents/claim", nil)
		if tc.header != "" {
			r.Header.Set("X-Forwarded-Proto", tc.header)
		}
		if got := requestScheme(r); got != tc.want {
			t.Errorf("X-Forwarded-Proto %q: scheme = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// The clock rides the claim and reaches the page, so an operator can see that
// a Mac's clock has drifted before they wonder why a build's last step claims
// to have taken minus four minutes.
func TestTheAgentClockReachesTheFleet(t *testing.T) {
	s, mux := newAgentServer(t)
	s.Agents = agents.NewRegistry()
	macProject(t, s)

	skewed := time.Now().Add(20 * time.Minute).UTC()
	w := doJSON(t, mux, "POST", "/api/agents/claim", map[string]interface{}{
		"agent": "mac-1", "executors": []string{"mac"}, "clock": skewed,
	})
	if w.Code != 204 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}

	w = doJSON(t, mux, "GET", "/api/agents", nil)
	var fleet agents.Fleet
	decodeJSON(t, w, &fleet)
	a := fleet.Agents[0]
	if !a.ClockKnown {
		t.Fatal("the reported clock did not reach the page")
	}
	if !a.ClockOff {
		t.Errorf("a twenty-minute skew was not flagged (skew %q)", a.ClockSkew)
	}
	if a.ClockSkew == "" {
		t.Error("no skew figure; which way it is wrong is the first thing anybody asks")
	}
}

// An agent too old to send a clock must claim exactly as before, and must not
// appear to agree perfectly.
func TestAnAgentThatSendsNoClockIsUnaffected(t *testing.T) {
	s, mux := newAgentServer(t)
	s.Agents = agents.NewRegistry()
	macProject(t, s)

	if w := doJSON(t, mux, "POST", "/api/agents/claim",
		map[string]interface{}{"agent": "mac-1", "executors": []string{"mac"}}); w.Code != 204 {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	w := doJSON(t, mux, "GET", "/api/agents", nil)
	var fleet agents.Fleet
	decodeJSON(t, w, &fleet)
	if fleet.Agents[0].ClockKnown {
		t.Error("an agent that sent no clock was treated as having reported one")
	}
}
