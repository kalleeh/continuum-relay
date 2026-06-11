package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nhooyr.io/websocket"

	"github.com/continuum-app/continuum-relay/internal/apns"
	"github.com/continuum-app/continuum-relay/internal/auth"
	"github.com/continuum-app/continuum-relay/internal/peers"
	"github.com/continuum-app/continuum-relay/internal/sysinfo"
)

func newClientID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type Server struct {
	hub        *Hub
	auth       *auth.Authenticator
	broker     *PermissionBroker
	peers      *peers.Manager
	addr       string
	listener   net.Listener
	serverInfo sysinfo.Info
}

// NewServer builds the relay HTTP/WebSocket server. The authenticator is passed
// in (rather than constructed here) so the terminal server can share the same
// instance — that gives the PTY endpoint the same per-IP lockout and, crucially,
// makes rotate_token (which calls authenticator.UpdateToken) take effect on the
// terminal endpoint too, instead of leaving the old token valid there until restart.
func NewServer(addr string, authenticator *auth.Authenticator, apnsClient *apns.Client, peersMgr *peers.Manager, listener net.Listener) *Server {
	return &Server{
		hub:        NewHub(apnsClient),
		auth:       authenticator,
		broker:     NewPermissionBroker(),
		peers:      peersMgr,
		addr:       addr,
		listener:   listener,
		serverInfo: sysinfo.Detect(),
	}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/api/chat", s.handleChatProxy)
	mux.HandleFunc("/api/permission", s.handlePermissionResponse)
	mux.HandleFunc("/api/peers", s.handlePeers)
	mux.HandleFunc("/api/info", s.handleInfo)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
	slog.Info("continuum-relay listening", "addr", s.addr)

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	if s.listener != nil {
		err := srv.Serve(s.listener)
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}

	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateRequest(r) {
		slog.Warn("auth failed", "ip", r.RemoteAddr)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // WireGuard handles transport security
	})
	if err != nil {
		slog.Error("websocket accept failed", "err", err)
		return
	}
	conn.SetReadLimit(1 << 20) // 1MB per message
	defer conn.CloseNow()

	clientID := newClientID()
	slog.Info("client authenticated", "id", clientID, "ip", r.RemoteAddr)

	HandleClient(r.Context(), conn, s.hub, s.auth, clientID)

	conn.Close(websocket.StatusNormalClosure, "")
}

func (s *Server) handlePermissionResponse(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID    string `json:"id"`
		Allow bool   `json:"allow"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Bind to the originating client: Respond is a no-op unless this IP is the
	// one that registered the request, so another device sharing the token
	// can't approve a prompt for this client's session. Always return 200 so a
	// rejected attempt can't probe which IDs exist.
	if !s.broker.Respond(req.ID, auth.ClientIP(r), req.Allow) {
		slog.Warn("permission response rejected (unknown id or wrong client)", "ip", r.RemoteAddr)
	}
	w.WriteHeader(http.StatusOK)
}

// handleSessions exposes the relay's current session list over HTTP (read-only).
// This is what the `continuum-relay sessions` CLI queries, so checking sessions
// never has to spawn a second relay process.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.hub.ListSessions())
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.serverInfo)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	if s.peers == nil {
		http.Error(w, "peer management not available", http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		list, err := s.peers.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)

	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			req.Name = "device"
		}
		if req.Name == "" {
			req.Name = "device"
		}
		result, err := s.peers.Add(req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(result)

	case http.MethodDelete:
		// Expect index in query: DELETE /api/peers?index=2
		idxStr := r.URL.Query().Get("index")
		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < 1 {
			http.Error(w, "invalid index parameter", http.StatusBadRequest)
			return
		}
		if err := s.peers.Remove(idx); err != nil {
			status := http.StatusInternalServerError
			if strings.Contains(err.Error(), "out of range") {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
