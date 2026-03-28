package logging

import (
	"log/slog"
	"os"
)

func Setup(logPath string) {
	var handler slog.Handler
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
		if err == nil {
			handler = slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})
		}
	}
	if handler == nil {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	}
	slog.SetDefault(slog.New(handler))
}
