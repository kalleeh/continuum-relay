package main

import (
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/continuum-app/continuum-relay/internal/apns"
	"github.com/continuum-app/continuum-relay/internal/logging"
	"github.com/continuum-app/continuum-relay/internal/relay"
	"github.com/continuum-app/continuum-relay/internal/terminal"
	"github.com/continuum-app/continuum-relay/internal/wg"
)

func main() {
	token := os.Getenv("CONTINUUM_TOKEN")
	if token == "" {
		slog.Error("CONTINUUM_TOKEN environment variable is required")
		os.Exit(1)
	}

	addr := os.Getenv("CONTINUUM_RELAY_ADDR")
	if addr == "" {
		addr = "10.100.0.1:7682"
	}

	logPath := os.Getenv("CONTINUUM_RELAY_LOG")
	if logPath == "" {
		logPath = "/var/log/continuum/relay.log"
	}

	logging.Setup(logPath)
	slog.Info("starting continuum-relay", "addr", addr)

	// ── WireGuard ────────────────────────────────────────────────────────────
	// Embedded WireGuard server. Reads /etc/wireguard/wg0.conf (or
	// CONTINUUM_WG_CONFIG). Skip with CONTINUUM_WG_DISABLED=1 (e.g. if
	// WireGuard is managed externally, or during local dev without root).
	if os.Getenv("CONTINUUM_WG_DISABLED") != "1" {
		wgConfPath := os.Getenv("CONTINUUM_WG_CONFIG")
		if wgConfPath == "" {
			wgConfPath = "/etc/wireguard/wg0.conf"
		}
		cfg, err := wg.ParseFile(wgConfPath)
		if err != nil {
			slog.Warn("WireGuard config not found — tunnel skipped", "path", wgConfPath, "err", err)
		} else {
			wgServer, err := wg.New(cfg)
			if err != nil {
				slog.Error("WireGuard init failed", "err", err)
				os.Exit(1)
			}
			if err := wgServer.Start(); err != nil {
				slog.Error("WireGuard start failed (needs root/CAP_NET_ADMIN)", "err", err)
				os.Exit(1)
			}
			defer wgServer.Close()
			slog.Info("WireGuard tunnel active", "ip", wgServer.InterfaceIP())
		}
	} else {
		slog.Info("WireGuard disabled (CONTINUUM_WG_DISABLED=1)")
	}

	// ── Terminal server (replaces ttyd) ──────────────────────────────────────
	// Listens on port 7681, speaks the xterm.js/ttyd binary WebSocket protocol.
	// Each connection gets its own PTY running CONTINUUM_TERMINAL_CMD.
	termCmd := strings.Fields(os.Getenv("CONTINUUM_TERMINAL_CMD"))
	if len(termCmd) == 0 {
		termCmd = []string{"tmux", "new-session", "-A", "-s", "main"}
	}
	termAddr := os.Getenv("CONTINUUM_TERMINAL_ADDR")
	if termAddr == "" {
		// Derive from relay addr: same host, port 7681
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			slog.Warn("could not parse relay addr for terminal addr derivation, using default", "addr", addr, "err", err)
			host = "10.100.0.1"
		}
		termAddr = net.JoinHostPort(host, "7681")
	}
	termServer := terminal.New(termAddr, token, termCmd)
	go func() {
		if err := termServer.Run(); err != nil {
			slog.Error("terminal server exited", "err", err)
		}
	}()

	// ── Claude Code relay ─────────────────────────────────────────────────────
	apnsClient := buildAPNSClient()
	server := relay.NewServer(addr, token, apnsClient)
	if err := server.Run(); err != nil {
		slog.Error("relay server exited", "err", err)
		os.Exit(1)
	}
}

func buildAPNSClient() *apns.Client {
	keyPath := os.Getenv("APNS_KEY_PATH")
	keyID := os.Getenv("APNS_KEY_ID")
	teamID := os.Getenv("APNS_TEAM_ID")
	bundleID := os.Getenv("APNS_BUNDLE_ID")

	if keyPath == "" || keyID == "" || teamID == "" || bundleID == "" {
		slog.Info("APNs not configured — push notifications disabled")
		return nil
	}

	client, err := apns.New(keyPath, keyID, teamID, bundleID)
	if err != nil {
		slog.Error("APNs client init failed — push disabled", "err", err)
		return nil
	}
	slog.Info("APNs push enabled", "bundle_id", bundleID)
	return client
}
