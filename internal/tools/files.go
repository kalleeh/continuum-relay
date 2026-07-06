package tools

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	maxReadBytes  = 10 * 1024  // 10KB
	maxWriteBytes = 100 * 1024 // 100KB
)

// sensitivePatterns are path components that must never be read or written.
// Checked against every component of the resolved path.
var sensitivePatterns = []string{
	".ssh",
	".aws",
	".gnupg",
	".env",
	".git/config",
	".netrc",
	".docker",
	".kube",
	".config/gh",
	"id_rsa",
	"id_ed25519",
	"authorized_keys",
	"known_hosts",
	// Package manager / DB client credential files.
	".npmrc",
	".pgpass",
	".my.cnf",
	".pypirc",
	".cargo/credentials",
	// Cloud provider credential/config files.
	".azure",
	"gcloud/credentials",
	"gcloud/legacy_credentials",
	"service-account.json",
	"service_account.json",
	".boto",
	// Infra-as-code state (often contains plaintext secrets/outputs).
	"terraform.tfstate",
	".terraform",
	// Generic secret-shaped filenames.
	".htpasswd",
	"credentials.json",
	"secrets.yaml",
	"secrets.yml",
}

// sensitiveExactPaths are full paths that must never be accessed.
var sensitiveExactPaths = []string{
	"/etc/shadow",
	"/etc/passwd",
	"/etc/continuum/env",
	"/etc/wireguard",
}

// validatePath checks that the path is absolute, resolves symlinks, verifies
// the resolved path is under allowed directories, and rejects sensitive paths.
func validatePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", &pathError{"path must be absolute"}
	}
	if strings.Contains(path, "..") {
		return "", &pathError{"path traversal not allowed"}
	}

	// Clean the path first
	clean := filepath.Clean(path)

	// Check prefix BEFORE symlink resolution (defense in depth)
	if !strings.HasPrefix(clean, "/home/") && !strings.HasPrefix(clean, "/tmp/") {
		return "", &pathError{"path must be under /home/ or /tmp/"}
	}

	// Resolve symlinks — the resolved path must ALSO be under allowed prefixes
	resolved, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		// Directory doesn't exist yet — for reads this is an error,
		// for writes we check the clean path only
		resolved = filepath.Dir(clean)
	}
	resolved = filepath.Join(resolved, filepath.Base(clean))

	if !strings.HasPrefix(resolved, "/home/") && !strings.HasPrefix(resolved, "/tmp/") {
		return "", &pathError{"resolved path escapes allowed directory"}
	}

	// Check sensitive patterns against both clean and resolved paths
	for _, pattern := range sensitivePatterns {
		if strings.Contains(clean, pattern) || strings.Contains(resolved, pattern) {
			return "", &pathError{"access to sensitive path denied"}
		}
	}
	for _, exact := range sensitiveExactPaths {
		if strings.HasPrefix(clean, exact) || strings.HasPrefix(resolved, exact) {
			return "", &pathError{"access to sensitive path denied"}
		}
	}

	return resolved, nil
}

type pathError struct{ msg string }

func (e *pathError) Error() string { return e.msg }

// executeReadFile reads a file after validating and resolving the path.
func executeReadFile(path string) ToolResult {
	resolved, err := validatePath(path)
	if err != nil {
		return ToolResult{Name: "read_file", Error: err.Error()}
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return ToolResult{Name: "read_file", Error: "file not found"}
	}
	// Reject symlinks at the final target too
	if info.Mode()&os.ModeSymlink != 0 {
		return ToolResult{Name: "read_file", Error: "symlinks not allowed"}
	}
	if info.IsDir() {
		return ToolResult{Name: "read_file", Error: "path is a directory"}
	}
	if info.Size() > maxReadBytes {
		return ToolResult{Name: "read_file", Error: "file exceeds 10KB limit"}
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return ToolResult{Name: "read_file", Error: "read failed"}
	}
	return ToolResult{Name: "read_file", Content: string(data)}
}

// executeWriteFile writes content to a file after validating the path and size.
// SECURITY: This function must ONLY be called via ExecuteUnsafe() after
// explicit user permission has been obtained. It is never reachable from Execute().
func executeWriteFile(path, content string) ToolResult {
	resolved, err := validatePath(path)
	if err != nil {
		return ToolResult{Name: "write_file", Error: err.Error()}
	}
	if len(content) > maxWriteBytes {
		return ToolResult{Name: "write_file", Error: "content exceeds 100KB limit"}
	}
	// Reject a symlink at the target, mirroring the read path. Without this a
	// pre-planted symlink (e.g. ~/notes.txt -> ~/.ssh/authorized_keys) passes
	// the prefix/sensitive-pattern checks on its own innocuous path string, and
	// os.WriteFile would follow it and clobber the real target.
	if info, err := os.Lstat(resolved); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return ToolResult{Name: "write_file", Error: "symlinks not allowed"}
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0755); err != nil {
		return ToolResult{Name: "write_file", Error: "failed to create directory"}
	}
	// O_NOFOLLOW closes the TOCTOU window: if the target is swapped for a symlink
	// between the Lstat above and the open, the open fails rather than following it.
	f, err := os.OpenFile(resolved, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0644)
	if err != nil {
		return ToolResult{Name: "write_file", Error: "write failed"}
	}
	defer f.Close()
	if _, err := f.Write([]byte(content)); err != nil {
		return ToolResult{Name: "write_file", Error: "write failed"}
	}
	return ToolResult{Name: "write_file", Content: "wrote " + path}
}
