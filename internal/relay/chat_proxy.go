package relay

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/continuum-app/continuum-relay/internal/auth"
	"github.com/continuum-app/continuum-relay/internal/tools"
)

// No client-level Timeout — each request uses its own context deadline
// (3 min for chat proxy). A client-level timeout would cut off long
// tool-calling rounds that legitimately take >60s.
var ollamaClient = &http.Client{}

type ollamaChatRequest struct {
	Model     string                 `json:"model"`
	Messages  []ollamaMessage        `json:"messages"`
	Stream    bool                   `json:"stream"`
	Think     *bool                  `json:"think,omitempty"`
	Options   json.RawMessage        `json:"options,omitempty"`
	KeepAlive string                 `json:"keep_alive,omitempty"`
	Tools     []tools.ToolDefinition `json:"tools,omitempty"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaToolCallFn `json:"function"`
}

type ollamaToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

type toolEvent struct {
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	Result string          `json:"result"`
}

func ollamaHost() string {
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		return h
	}
	return "http://10.100.0.1:11434"
}

func (s *Server) handleChatProxy(w http.ResponseWriter, r *http.Request) {
	if !s.auth.ValidateRequest(r) {
		slog.Warn("chat proxy auth failed", "ip", r.RemoteAddr)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req ollamaChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	req.Tools = tools.AllTools()
	host := ollamaHost()

	// Start streaming response immediately so we can send permission requests inline
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Transfer-Encoding", "chunked")

	var events []toolEvent

	// Tool-calling loop: non-streaming rounds with inline permission requests
	const maxRounds = 10
	for i := range maxRounds {
		req.Stream = false

		payload, _ := json.Marshal(req)
		ollamaReq, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/chat", bytes.NewReader(payload))
		if err != nil {
			slog.Error("failed to create ollama request", "round", i, "err", err)
			return
		}
		ollamaReq.Header.Set("Content-Type", "application/json")
		resp, err := ollamaClient.Do(ollamaReq)
		if err != nil {
			slog.Error("ollama request failed", "round", i, "err", err)
			return
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var chatResp ollamaChatResponse
		if json.Unmarshal(respBody, &chatResp) != nil {
			return
		}

		if len(chatResp.Message.ToolCalls) == 0 {
			req.Messages = append(req.Messages, chatResp.Message)
			break
		}

		req.Messages = append(req.Messages, chatResp.Message)

		for _, tc := range chatResp.Message.ToolCalls {
			name := tc.Function.Name
			call := tools.ToolCall{Name: name, Args: tc.Function.Arguments}

			var result tools.ToolResult
			if tools.SafeTools[name] {
				result = tools.Execute(call)
			} else {
				// Send permission request inline to iOS client. Bind it to this
				// client's IP so only the originating device can approve it.
				allowed := s.requestInlinePermission(ctx, w, flusher, auth.ClientIP(r), name, tc.Function.Arguments)
				if allowed {
					slog.Info("tool permission granted", "name", name)
					result = tools.ExecuteUnsafe(call)
				} else {
					slog.Info("tool permission denied", "name", name)
					result = tools.ToolResult{Name: name, Error: "User denied permission"}
				}
			}

			content := result.Content
			if result.Error != "" {
				content = "error: " + result.Error
			}
			events = append(events, toolEvent{Name: name, Args: tc.Function.Arguments, Result: content})
			req.Messages = append(req.Messages, ollamaMessage{Role: "tool", Content: content})
		}
	}

	// Send tool events summary
	if len(events) > 0 {
		line, _ := json.Marshal(map[string][]toolEvent{"tool_events": events})
		fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()
	}

	// Final streaming response
	req.Stream = true
	req.Tools = nil
	payload, _ := json.Marshal(req)
	ollamaReq, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return
	}
	ollamaReq.Header.Set("Content-Type", "application/json")
	resp, err := ollamaClient.Do(ollamaReq)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flusher.Flush()
		}
		if err != nil {
			break
		}
	}
}

// requestInlinePermission sends a permission request as an NDJSON line in the
// HTTP streaming response, then waits for the response via /api/permission.
func (s *Server) requestInlinePermission(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, ownerIP, toolName string, args json.RawMessage) bool {
	id := randomPermID()

	ch := s.broker.RegisterPending(id, ownerIP)
	defer s.broker.RemovePending(id)

	line, _ := json.Marshal(map[string]any{
		"tool_permission_request": map[string]any{
			"id":        id,
			"tool_name": toolName,
			"arguments": args,
		},
	})
	fmt.Fprintf(w, "%s\n", line)
	flusher.Flush()

	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	select {
	case allowed := <-ch:
		return allowed
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func randomPermID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}
