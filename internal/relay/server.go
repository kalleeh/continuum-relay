package relay

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"

	"nhooyr.io/websocket"

	"github.com/continuum-app/continuum-relay/internal/apns"
	"github.com/continuum-app/continuum-relay/internal/auth"
)

func newClientID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type Server struct {
	hub  *Hub
	auth *auth.Authenticator
	addr string
}

func NewServer(addr, token string, apnsClient *apns.Client) *Server {
	return &Server{
		hub:  NewHub(apnsClient),
		auth: auth.New(token),
		addr: addr,
	}
}

func (s *Server) Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:        s.addr,
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute,
	}
	slog.Info("continuum-relay listening", "addr", s.addr)
	return srv.ListenAndServe()
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
