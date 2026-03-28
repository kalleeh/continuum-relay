package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"nhooyr.io/websocket"

	"github.com/continuum-app/continuum-relay/internal/auth"
	"github.com/continuum-app/continuum-relay/internal/projects"
)

var (
	sessionNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	tokenRe       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type ClientMessage struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	CWD         string `json:"cwd,omitempty"`
	SessionType string `json:"sessionType,omitempty"`
	Session     string `json:"session,omitempty"`
	Content     string `json:"content,omitempty"`
	GitHubToken string `json:"githubToken,omitempty"`
	Slug        string `json:"slug,omitempty"`
	ID          string `json:"id,omitempty"`
	Allow       bool   `json:"allow,omitempty"`
	Token       string `json:"token,omitempty"`       // rotate_token
	DeviceToken string `json:"device_token,omitempty"` // register_device
}

func HandleClient(ctx context.Context, conn *websocket.Conn, hub *Hub, authenticator *auth.Authenticator, clientID string) {
	slog.Info("client connected", "id", clientID)
	defer slog.Info("client disconnected", "id", clientID)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu                sync.Mutex
		subscribedSession *Session
		outputCh          <-chan []byte
	)

	unsubscribe := func() {
		mu.Lock()
		defer mu.Unlock()
		if subscribedSession != nil {
			subscribedSession.Unsubscribe(clientID)
			subscribedSession = nil
			outputCh = nil
		}
	}
	defer unsubscribe()

	// Goroutine: forward session output → WebSocket
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		for {
			mu.Lock()
			ch := outputCh
			mu.Unlock()
			if ch == nil {
				select {
				case <-ctx.Done():
					return
				default:
					runtime.Gosched()
					continue
				}
			}
			select {
			case data, ok := <-ch:
				if !ok {
					mu.Lock()
					outputCh = nil
					mu.Unlock()
					continue
				}
				if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Main loop: read client messages
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			cancel()
			break
		}
		var msg ClientMessage
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}

		switch msg.Type {
		case "list_sessions":
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())

		case "create_session":
			if !sessionNameRe.MatchString(msg.Name) {
				writeError(ctx, conn, "invalid_name", "Session name invalid")
				continue
			}
			s, err := hub.CreateSession(msg.Name, msg.CWD, msg.SessionType)
			if err != nil {
				writeError(ctx, conn, "create_failed", err.Error())
				continue
			}
			unsubscribe()
			mu.Lock()
			subscribedSession = s
			outputCh = s.Subscribe(clientID)
			mu.Unlock()
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())

		case "attach_session":
			s, ok := hub.GetSession(msg.Name)
			if !ok {
				writeError(ctx, conn, "not_found", "Session not found")
				continue
			}
			unsubscribe()
			mu.Lock()
			subscribedSession = s
			outputCh = s.Subscribe(clientID)
			mu.Unlock()

		case "detach_session":
			unsubscribe()

		case "send_message":
			if s, ok := hub.GetSession(msg.Session); ok {
				_ = s.Send(msg.Content)
			}

		case "interrupt":
			if s, ok := hub.GetSession(msg.Session); ok {
				s.Interrupt()
			}

		case "permission_response":
			if s, ok := hub.GetSession(msg.Session); ok {
				s.RespondToPermission(msg.ID, msg.Allow)
			}

		case "delete_session":
			hub.DeleteSession(msg.Name)
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())

		case "register_device":
			if msg.DeviceToken != "" {
				hub.RegisterDevice(msg.DeviceToken)
			}

		case "rotate_token":
			if !tokenRe.MatchString(msg.Token) {
				writeError(ctx, conn, "invalid_token", "token must be 64 lowercase hex chars")
				continue
			}
			if err := rewriteEnvFile(msg.Token); err != nil {
				slog.Error("failed to rewrite env file during token rotation", "err", err)
				writeError(ctx, conn, "rotate_failed", fmt.Sprintf("env file rewrite failed: %v", err))
				continue
			}
			authenticator.UpdateToken(msg.Token)
			slog.Info("auth token rotated", "client_id", clientID)
			writeJSON(ctx, conn, map[string]string{"type": "token_rotated"})

		case "list_projects":
			records, err := projects.ListProjects()
			if err != nil {
				writeError(ctx, conn, "list_failed", err.Error())
				continue
			}
			writeJSON(ctx, conn, map[string]any{"type": "project_list", "projects": records})

		case "sync_project":
			if msg.Slug == "" {
				writeError(ctx, conn, "invalid_slug", "slug required")
				continue
			}
			if msg.GitHubToken == "" {
				writeError(ctx, conn, "missing_token", "githubToken required")
				continue
			}
			slug := msg.Slug
			token := msg.GitHubToken
			writeJSON(ctx, conn, map[string]string{"type": "project_sync_started", "slug": slug})
			go func() {
				err := projects.SyncProject(slug, token)
				if err != nil {
					writeJSON(ctx, conn, map[string]string{
						"type": "project_sync_error", "slug": slug, "message": err.Error(),
					})
				} else {
					parts := strings.SplitN(slug, "/", 2)
					name := parts[len(parts)-1]
					writeJSON(ctx, conn, map[string]string{
						"type": "project_sync_ok", "slug": slug, "name": name,
					})
				}
			}()

		case "remove_project":
			if err := projects.RemoveProject(msg.Name); err != nil {
				writeError(ctx, conn, "remove_failed", err.Error())
				continue
			}
			writeJSON(ctx, conn, map[string]string{"type": "project_removed", "name": msg.Name})
		}
	}
	<-outDone
}

// rewriteEnvFile atomically rewrites /etc/continuum/env with the new token.
func rewriteEnvFile(newToken string) error {
	const envPath = "/etc/continuum/env"
	content := "CONTINUUM_TOKEN=" + newToken + "\n"
	tmp := envPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, envPath)
}

func writeError(ctx context.Context, conn *websocket.Conn, code, message string) {
	data, _ := json.Marshal(map[string]string{
		"type":    "error",
		"code":    code,
		"message": message,
	})
	_ = conn.Write(ctx, websocket.MessageText, data)
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) {
	data, _ := json.Marshal(v)
	_ = conn.Write(ctx, websocket.MessageText, data)
}
