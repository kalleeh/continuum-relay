// Package tools defines tool schemas for Ollama tool calling and executes them server-side.
package tools

import "encoding/json"

// ToolDefinition matches Ollama's tool JSON schema format.
type ToolDefinition struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

// Function describes a callable tool function.
type Function struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Parameters  Parameters `json:"parameters"`
}

// Parameters describes the input schema for a tool function.
type Parameters struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties"`
	Required   []string            `json:"required"`
}

// Property describes a single parameter within a tool's schema.
type Property struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
}

// ToolCall represents an incoming tool invocation from the model.
type ToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"arguments"`
}

// ToolResult represents the outcome of executing a tool.
type ToolResult struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// AllTools returns the full set of tool definitions for Ollama.
func AllTools() []ToolDefinition {
	return []ToolDefinition{
		{
			Type: "function",
			Function: Function{
				Name:        "web_search",
				Description: "Search the web for information",
				Parameters: Parameters{
					Type: "object",
					Properties: map[string]Property{
						"query": {Type: "string", Description: "The search query"},
					},
					Required: []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: Function{
				Name:        "run_code",
				Description: "Execute code in a sandboxed environment",
				Parameters: Parameters{
					Type: "object",
					Properties: map[string]Property{
						"language": {Type: "string", Description: "Programming language", Enum: []string{"python", "bash"}},
						"code":     {Type: "string", Description: "The code to execute"},
					},
					Required: []string{"language", "code"},
				},
			},
		},
		{
			Type: "function",
			Function: Function{
				Name:        "web_fetch",
				Description: "Fetch a web page and extract its main content as markdown",
				Parameters: Parameters{
					Type: "object",
					Properties: map[string]Property{
						"url": {Type: "string", Description: "The URL to fetch"},
					},
					Required: []string{"url"},
				},
			},
		},
		{
			Type: "function",
			Function: Function{
				Name:        "read_file",
				Description: "Read the contents of a file",
				Parameters: Parameters{
					Type: "object",
					Properties: map[string]Property{
						"path": {Type: "string", Description: "Absolute file path to read"},
					},
					Required: []string{"path"},
				},
			},
		},
		{
			Type: "function",
			Function: Function{
				Name:        "write_file",
				Description: "Write content to a file",
				Parameters: Parameters{
					Type: "object",
					Properties: map[string]Property{
						"path":    {Type: "string", Description: "Absolute file path to write"},
						"content": {Type: "string", Description: "Content to write to the file"},
					},
					Required: []string{"path", "content"},
				},
			},
		},
	}
}

// SafeTools are tools that can be executed without user permission.
var SafeTools = map[string]bool{
	"web_search": true,
	"web_fetch":  true,
	"read_file":  true,
}

// Execute runs ONLY safe (read-only) tools. Dangerous tools (run_code, write_file)
// are rejected with a clear message. This is the ONLY entry point the proxy should use.
func Execute(call ToolCall) ToolResult {
	if !SafeTools[call.Name] {
		return ToolResult{
			Name:  call.Name,
			Error: "Tool " + call.Name + " requires user approval. Describe what you would do instead.",
		}
	}

	switch call.Name {
	case "web_search":
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return ToolResult{Name: call.Name, Error: "invalid arguments"}
		}
		if len(args.Query) > 500 {
			return ToolResult{Name: call.Name, Error: "query too long"}
		}
		return executeWebSearch(args.Query)

	case "web_fetch":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return ToolResult{Name: call.Name, Error: "invalid arguments"}
		}
		return executeWebFetch(args.URL)

	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return ToolResult{Name: call.Name, Error: "invalid arguments"}
		}
		return executeReadFile(args.Path)

	default:
		return ToolResult{Name: call.Name, Error: "unknown tool"}
	}
}

// ExecuteUnsafe runs dangerous tools (run_code, write_file) that require
// prior user permission. The caller MUST have obtained explicit user approval
// before calling this function. This is never called from Execute().
func ExecuteUnsafe(call ToolCall) ToolResult {
	switch call.Name {
	case "run_code":
		var args struct {
			Language string `json:"language"`
			Code     string `json:"code"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return ToolResult{Name: call.Name, Error: "invalid arguments"}
		}
		return executeRunCode(args.Language, args.Code)
	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(call.Args, &args); err != nil {
			return ToolResult{Name: call.Name, Error: "invalid arguments"}
		}
		return executeWriteFile(args.Path, args.Content)
	default:
		return ToolResult{Name: call.Name, Error: "unknown unsafe tool"}
	}
}
