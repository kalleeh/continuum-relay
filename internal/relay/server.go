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

func NewServer(addr, token string, apnsClient *apns.Client, peersMgr *peers.Manager, listener net.Listener) *Server {
	return &Server{
		hub:        NewHub(apnsClient),
		auth:       auth.New(token),
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

	HandleClient(r.Context(), conn, s.hub, s.auth, s.broker, clientID)

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

	s.broker.Respond(req.ID, req.Allow)
	w.WriteHeader(http.StatusOK)
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
