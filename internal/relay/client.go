package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"nhooyr.io/websocket"

	"github.com/continuum-app/continuum-relay/internal/auth"
	"github.com/continuum-app/continuum-relay/internal/projects"
)

var (
	sessionNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	tokenRe       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	apnsTokenRe   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	// Live Activity push tokens are hex but longer/variable vs device tokens.
	activityTokenRe = regexp.MustCompile(`^[0-9a-fA-F]{64,512}$`)
)

type ClientMessage struct {
	Type          string `json:"type"`
	Name          string `json:"name,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	SessionType   string `json:"sessionType,omitempty"`
	Session       string `json:"session,omitempty"`
	GitHubToken   string `json:"githubToken,omitempty"`
	Slug          string `json:"slug,omitempty"`
	ID            string `json:"id,omitempty"`              // tool_permission_response
	Allow         bool   `json:"allow,omitempty"`           // tool_permission_response
	Token         string `json:"token,omitempty"`           // rotate_token
	DeviceToken   string `json:"device_token,omitempty"`    // register_device
	ActivityToken string `json:"activity_token,omitempty"`  // register_activity (Live Activity push token)
	Force         bool   `json:"force,omitempty"`           // remove_project: delete even with unsaved work
	Key           string `json:"key,omitempty"`             // send_key: up | down | enter | escape | one of a-z0-9
	Lines         int    `json:"lines,omitempty"`           // capture_screen: how many trailing lines
	Text          string `json:"text,omitempty"`            // send_text: typed into the session, no Enter
	Agent         string `json:"agent,omitempty"`           // start_session: claude | kiro | terminal
	Project       string `json:"project,omitempty"`         // start_session: one of the names list_projects returns
	Status        bool   `json:"status,omitempty"`          // list_sessions: also send each session's working/idle state
	History       int    `json:"history,omitempty"`         // capture_screen (styled): also read this many lines of scrollback
	Styled        bool   `json:"styled,omitempty"`          // capture_screen: keep colours, drop the input box frame, add "activity"
	Skip          bool   `json:"skipPermissions,omitempty"` // start_session: run the agent without asking before tools
}

func HandleClient(ctx context.Context, conn *websocket.Conn, hub *Hub, authenticator *auth.Authenticator, clientID string) {
	slog.Info("client connected", "id", clientID)
	defer slog.Info("client disconnected", "id", clientID)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register this client for hub-wide broadcasts (session_status from the
	// detector).
	statusCh := make(chan []byte, 16)
	hub.RegisterClient(clientID, statusCh)
	defer hub.UnregisterClient(clientID)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-statusCh:
				if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Goroutine: WebSocket ping/pong heartbeat
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.Ping(ctx); err != nil {
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
		case "hello":
			writeJSON(ctx, conn, helloMessage())

		case "list_sessions":
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())
			// A client that asks ("status": true; the desk device does) also gets each session's
			// current working/idle state, which is otherwise only pushed when it changes. Other
			// clients get exactly what they always did.
			if msg.Status {
				for _, status := range hub.StatusSnapshot() {
					_ = conn.Write(ctx, websocket.MessageText, status)
				}
			}

		case "create_session":
			if !sessionNameRe.MatchString(msg.Name) {
				writeError(ctx, conn, "invalid_name", "Session name invalid")
				continue
			}
			if _, err := hub.CreateSession(msg.Name, msg.CWD, msg.SessionType); err != nil {
				writeError(ctx, conn, "create_failed", err.Error())
				continue
			}
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())

		case "attach_session":
			if !sessionNameRe.MatchString(msg.Name) {
				writeError(ctx, conn, "invalid_name", "Session name invalid")
				continue
			}
			if _, ok := hub.GetSession(msg.Name); !ok {
				writeError(ctx, conn, "not_found", "Session not found")
				continue
			}

		case "delete_session":
			if !sessionNameRe.MatchString(msg.Name) {
				writeError(ctx, conn, "invalid_name", "Session name invalid")
				continue
			}
			hub.DeleteSession(msg.Name)
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())

		case "register_device":
			if msg.DeviceToken == "" || !apnsTokenRe.MatchString(msg.DeviceToken) {
				continue
			}
			hub.RegisterDevice(msg.DeviceToken)

		case "register_activity":
			// Live Activity push token for a session. Empty token unregisters
			// (activity ended). Tokens are hex but variable length, so validate
			// loosely (hex, sane bounds) rather than the fixed device-token regex.
			if !sessionNameRe.MatchString(msg.Session) {
				continue
			}
			if msg.ActivityToken != "" && !activityTokenRe.MatchString(msg.ActivityToken) {
				continue
			}
			hub.RegisterActivity(msg.Session, msg.ActivityToken)

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
					slog.Warn("project sync failed", "slug", slug, "err", err)
					writeJSON(ctx, conn, map[string]string{
						"type": "project_sync_error", "slug": slug, "message": "sync failed",
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
			if err := projects.RemoveProject(msg.Name, msg.Force); err != nil {
				// Distinct code for the recoverable "has unsaved work" case so
				// the client can offer a force-retry instead of a hard failure.
				code := "remove_failed"
				if errors.Is(err, projects.ErrUnsavedWork) {
					code = "remove_needs_force"
				}
				writeError(ctx, conn, code, err.Error())
				continue
			}
			writeJSON(ctx, conn, map[string]string{"type": "project_removed", "name": msg.Name})

		case "start_session":
			cwd := ""
			if msg.Project != "" {
				path, err := projectPath(msg.Project)
				if err != nil {
					writeError(ctx, conn, "start_failed", err.Error())
					continue
				}
				cwd = path
			}
			if err := hub.StartSession(msg.Name, cwd, msg.Agent, msg.Skip); err != nil {
				writeError(ctx, conn, "start_failed", err.Error())
				continue
			}
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())

		case "end_session":
			if err := hub.EndSession(msg.Name); err != nil {
				writeError(ctx, conn, "end_failed", err.Error())
				continue
			}
			_ = conn.Write(ctx, websocket.MessageText, hub.SessionListJSON())

		case "send_key":
			if err := hub.SendKey(msg.Session, msg.Key); err != nil {
				writeError(ctx, conn, "send_key_failed", err.Error())
			}

		case "send_text":
			if err := hub.SendText(msg.Session, msg.Text); err != nil {
				writeError(ctx, conn, "send_text_failed", err.Error())
			}

		case "capture_screen":
			if msg.Styled {
				screen, err := hub.CaptureScreenStyled(msg.Session, msg.History)
				if err != nil {
					writeError(ctx, conn, "capture_failed", err.Error())
					continue
				}
				writeJSON(ctx, conn, map[string]any{"type": "screen", "session": msg.Session, "lines": screen.Lines,
					"state": screen.State, "elapsed": screen.Elapsed, "meta": screen.Meta})
				continue
			}
			lines, err := hub.CaptureScreen(msg.Session, msg.Lines)
			if err != nil {
				writeError(ctx, conn, "capture_failed", err.Error())
				continue
			}
			writeJSON(ctx, conn, map[string]any{"type": "screen", "session": msg.Session, "lines": lines})

		case "project_status":
			st, err := projects.ProjectStatus(msg.Name)
			if err != nil {
				writeError(ctx, conn, "status_failed", err.Error())
				continue
			}
			writeJSON(ctx, conn, map[string]any{
				"type":        "project_status_result",
				"name":        st.Name,
				"isRepo":      st.IsRepo,
				"clean":       st.Clean,
				"uncommitted": st.Uncommitted,
				"untracked":   st.Untracked,
				"stashes":     st.Stashes,
				"unpushed":    st.Unpushed,
			})

			// Note: tool-permission responses come back over HTTP POST /api/permission
			// (handlePermissionResponse), not this WebSocket, so they can be bound to
			// the originating client's IP. There is intentionally no permission case here.
		}
	}
}

// rewriteEnvFile atomically updates CONTINUUM_TOKEN in /etc/continuum/env,
// preserving all other lines (comments, blank lines, other KEY=VALUE pairs).
func rewriteEnvFile(newToken string) error {
	const envPath = "/etc/continuum/env"

	data, err := os.ReadFile(envPath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, "CONTINUUM_TOKEN=") {
			lines[i] = "CONTINUUM_TOKEN=" + newToken
			found = true
			break
		}
	}
	if !found {
		// Insert before final empty element if file ended with newline.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = append(lines[:len(lines)-1], "CONTINUUM_TOKEN="+newToken, "")
		} else {
			lines = append(lines, "CONTINUUM_TOKEN="+newToken)
		}
	}

	tmp := envPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0600); err != nil {
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

// projectPath resolves a project name from list_projects to its folder. Only names the relay itself
// lists are accepted, so a client cannot point a session at an arbitrary directory.
func projectPath(name string) (string, error) {
	records, err := projects.ListProjects()
	if err != nil {
		return "", err
	}
	for _, r := range records {
		if r.Name == name {
			return r.Path, nil
		}
	}
	return "", errors.New("unknown project")
}
