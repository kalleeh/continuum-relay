package relay

import (
	"sync"
)

// PermissionBroker correlates a tool-permission prompt (sent inline in a chat
// HTTP response stream by chat_proxy.go) with the response the client POSTs back
// to /api/permission. Each pending request is bound to the IP of the client that
// originated it, so a second device sharing the token cannot approve a prompt
// destined for another client's session.
//
// There is deliberately no client registry / broadcast here: the prompt travels
// only down the originating client's own response stream, and the request ID is
// a 128-bit secret never logged or sent elsewhere. The earlier broadcast design
// (RequestPermission + a clients map) was removed — it sent every prompt to all
// connected clients, which is the cross-client-approval hole this binding closes.
type PermissionBroker struct {
	mu      sync.Mutex
	pending map[string]pendingPerm // requestID -> pending request
}

type pendingPerm struct {
	ch      chan bool
	ownerIP string // IP of the client that requested the prompt; only it may respond
}

func NewPermissionBroker() *PermissionBroker {
	return &PermissionBroker{
		pending: make(map[string]pendingPerm),
	}
}

// RegisterPending creates a pending permission channel bound to ownerIP and
// returns it. The caller is responsible for calling RemovePending when done.
func (b *PermissionBroker) RegisterPending(id, ownerIP string) <-chan bool {
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.pending[id] = pendingPerm{ch: ch, ownerIP: ownerIP}
	b.mu.Unlock()
	return ch
}

// RemovePending cleans up a pending permission request.
func (b *PermissionBroker) RemovePending(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// Respond resolves a pending permission request. It is a no-op (returning false)
// unless an entry with that ID exists AND responderIP matches the IP that
// registered it — preventing a different client from approving another's prompt.
// The bool reports whether the response was accepted, so the caller can log a
// rejected cross-client attempt without revealing whether the ID existed.
func (b *PermissionBroker) Respond(requestID, responderIP string, allow bool) bool {
	b.mu.Lock()
	p, ok := b.pending[requestID]
	b.mu.Unlock()
	if !ok || p.ownerIP != responderIP {
		return false
	}
	select {
	case p.ch <- allow:
	default:
	}
	return true
}
