package logging

import (
	"io"
	"log/slog"
	"os"

	"golang.org/x/term"
)

const maxLogSize = 50 * 1024 * 1024 // 50 MB

func Setup(logPath string) {
	level := slog.LevelInfo
	if os.Getenv("CONTINUUM_LOG_DEBUG") == "1" {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}

	if logPath == "" || logPath == "stderr" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
		return
	}

	// Rotate if too large
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(logPath, logPath+".1")
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
		slog.Warn("could not open log file, using stderr", "path", logPath, "err", err)
		return
	}

	// If stderr is a terminal (interactive), write to both file and stderr.
	// Otherwise (launchd/systemd), write to file only.
	var w io.Writer = f
	if term.IsTerminal(int(os.Stderr.Fd())) {
		w = io.MultiWriter(f, os.Stderr)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, opts)))
}
