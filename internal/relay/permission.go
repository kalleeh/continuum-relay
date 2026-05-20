package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// PermissionBroker routes tool permission requests from the chat proxy
// to connected WebSocket clients and waits for responses.
type PermissionBroker struct {
	mu      sync.Mutex
	pending map[string]chan bool     // requestID -> response channel
	clients map[string]func([]byte) // clientID -> send function
}

func NewPermissionBroker() *PermissionBroker {
	return &PermissionBroker{
		pending: make(map[string]chan bool),
		clients: make(map[string]func([]byte)),
	}
}

// RegisterClient adds a WebSocket client that can receive permission requests.
func (b *PermissionBroker) RegisterClient(id string, sendFn func([]byte)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[id] = sendFn
}

// UnregisterClient removes a WebSocket client.
func (b *PermissionBroker) UnregisterClient(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, id)
}

// RequestPermission sends a permission request to all connected clients
// and blocks until a response is received or the context expires.
// Returns true if allowed, false if denied or timed out.
func (b *PermissionBroker) RequestPermission(ctx context.Context, toolName string, args json.RawMessage) bool {
	id := randomID()
	ch := make(chan bool, 1)

	b.mu.Lock()
	b.pending[id] = ch
	msg, _ := json.Marshal(map[string]any{
		"type":      "tool_permission_request",
		"id":        id,
		"tool_name": toolName,
		"arguments": args,
	})
	for _, sendFn := range b.clients {
		sendFn(msg)
	}
	b.mu.Unlock()

	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	select {
	case allowed := <-ch:
		return allowed
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// RegisterPending creates a pending permission channel and returns it.
// The caller is responsible for calling RemovePending when done.
func (b *PermissionBroker) RegisterPending(id string) <-chan bool {
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.pending[id] = ch
	b.mu.Unlock()
	return ch
}

// RemovePending cleans up a pending permission request.
func (b *PermissionBroker) RemovePending(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// Respond handles a permission response from a WebSocket client.
func (b *PermissionBroker) Respond(requestID string, allow bool) {
	b.mu.Lock()
	ch, ok := b.pending[requestID]
	b.mu.Unlock()
	if ok {
		select {
		case ch <- allow:
		default:
		}
	}
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
