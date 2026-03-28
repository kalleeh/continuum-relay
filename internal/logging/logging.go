package logging

import (
	"log/slog"
	"os"
)

const maxLogSize = 50 * 1024 * 1024 // 50 MB

func Setup(logPath string) {
	if logPath == "" || logPath == "stderr" {
		// stderr: no rotation needed
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		return
	}

	// Rotate if too large
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(logPath, logPath+".1")
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		slog.Warn("could not open log file, using stderr", "path", logPath, "err", err)
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(f, nil)))
}
