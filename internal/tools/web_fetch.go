package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxFetchOutputBytes = 20 * 1024 // 20KB
const maxFetchURLLen = 2048

// validateFetchURL enforces a minimal contract on the model-supplied URL before
// it is forwarded: an absolute http/https URL with a host, within a length cap.
// The actual fetch is performed by Ollama's hosted servers (not the relay), so
// this is hygiene rather than an SSRF guard — it keeps file://, gopher://, data://
// and other non-web schemes out. If web_fetch is ever changed to fetch locally,
// a real SSRF guard (private-IP/metadata blocking after DNS resolution) MUST be
// added here first; see TestWebFetch_OnlyDialsOllama for the invariant that pins
// the current "relay never dials the target URL" behavior.
func validateFetchURL(rawURL string) error {
	if len(rawURL) > maxFetchURLLen {
		return fmt.Errorf("url too long")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("url must have a host")
	}
	return nil
}

// executeWebFetch calls Ollama's hosted web fetch API.
func executeWebFetch(rawURL string) ToolResult {
	if rawURL == "" {
		return ToolResult{Name: "web_fetch", Error: "url is required"}
	}
	if err := validateFetchURL(rawURL); err != nil {
		return ToolResult{Name: "web_fetch", Error: err.Error()}
	}
	key := ollamaAPIKey()
	if key == "" {
		return ToolResult{Name: "web_fetch", Error: "OLLAMA_API_KEY not configured"}
	}

	body, _ := json.Marshal(map[string]string{"url": rawURL})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://ollama.com/api/web_fetch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := ollamaAPIClient.Do(req)
	if err != nil {
		return ToolResult{Name: "web_fetch", Error: "fetch request failed"}
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if resp.StatusCode != 200 {
		return ToolResult{Name: "web_fetch", Error: fmt.Sprintf("fetch API returned %d", resp.StatusCode)}
	}

	var result struct {
		Title   string   `json:"title"`
		Content string   `json:"content"`
		Links   []string `json:"links"`
	}
	if json.Unmarshal(data, &result) != nil {
		return ToolResult{Name: "web_fetch", Error: "invalid response"}
	}

	var sb strings.Builder
	if result.Title != "" {
		fmt.Fprintf(&sb, "# %s\n\nSource: %s\n\n---\n\n", result.Title, rawURL)
	}
	sb.WriteString(result.Content)

	out := sb.String()
	if len(out) > maxFetchOutputBytes {
		out = out[:maxFetchOutputBytes] + "\n\n[truncated]"
	}
	return ToolResult{Name: "web_fetch", Content: out}
}
