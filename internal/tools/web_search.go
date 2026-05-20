package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var ollamaAPIClient = &http.Client{Timeout: 15 * time.Second}

func ollamaAPIKey() string { return os.Getenv("OLLAMA_API_KEY") }

// executeWebSearch calls Ollama's hosted web search API.
func executeWebSearch(query string) ToolResult {
	if query == "" {
		return ToolResult{Name: "web_search", Error: "query is required"}
	}
	key := ollamaAPIKey()
	if key == "" {
		return ToolResult{Name: "web_search", Error: "OLLAMA_API_KEY not configured"}
	}

	body, _ := json.Marshal(map[string]any{"query": query, "max_results": 5})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://ollama.com/api/web_search", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := ollamaAPIClient.Do(req)
	if err != nil {
		return ToolResult{Name: "web_search", Error: "search request failed"}
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != 200 {
		return ToolResult{Name: "web_search", Error: fmt.Sprintf("search API returned %d", resp.StatusCode)}
	}

	var result struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Results) == 0 {
		return ToolResult{Name: "web_search", Content: "No results found."}
	}

	var sb strings.Builder
	for i, r := range result.Results {
		fmt.Fprintf(&sb, "%d. **%s**\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Content)
	}
	return ToolResult{Name: "web_search", Content: sb.String()}
}
