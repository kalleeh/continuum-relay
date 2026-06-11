package relay

import "testing"

// TestRespond_MatchingOwnerResolves: the client that registered the request can
// resolve it, and the value propagates.
func TestRespond_MatchingOwnerResolves(t *testing.T) {
	b := NewPermissionBroker()
	ch := b.RegisterPending("id1", "10.0.0.1")
	if !b.Respond("id1", "10.0.0.1", true) {
		t.Fatal("Respond from owner IP returned false")
	}
	if v := <-ch; !v {
		t.Fatal("expected allow=true to propagate")
	}
}

// TestRespond_WrongClientRejected is the core of the client-binding fix: a
// different IP cannot approve another client's pending prompt, and the genuine
// owner can still resolve it afterward.
func TestRespond_WrongClientRejected(t *testing.T) {
	b := NewPermissionBroker()
	ch := b.RegisterPending("id1", "10.0.0.1")

	if b.Respond("id1", "10.0.0.99", true) {
		t.Fatal("Respond from a non-owner IP was accepted")
	}
	select {
	case <-ch:
		t.Fatal("channel resolved by a non-owner response")
	default:
	}

	if !b.Respond("id1", "10.0.0.1", false) {
		t.Fatal("owner could not resolve after a rejected foreign attempt")
	}
	if v := <-ch; v {
		t.Fatal("expected allow=false from the owner")
	}
}

// TestRespond_UnknownID is a no-op (e.g. an expired/already-resolved request).
func TestRespond_UnknownID(t *testing.T) {
	b := NewPermissionBroker()
	if b.Respond("nope", "10.0.0.1", true) {
		t.Fatal("Respond on an unknown ID was accepted")
	}
}

// TestRespond_AfterRemoveIsNoop: once removed (request finished/timed out), a
// late response must not be accepted — no replay.
func TestRespond_AfterRemoveIsNoop(t *testing.T) {
	b := NewPermissionBroker()
	b.RegisterPending("id1", "10.0.0.1")
	b.RemovePending("id1")
	if b.Respond("id1", "10.0.0.1", true) {
		t.Fatal("Respond accepted after RemovePending")
	}
}
