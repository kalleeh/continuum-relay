package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/continuum-app/continuum-relay/internal/apns"
	"github.com/continuum-app/continuum-relay/internal/logging"
	"github.com/continuum-app/continuum-relay/internal/peers"
	"github.com/continuum-app/continuum-relay/internal/relay"
	"github.com/continuum-app/continuum-relay/internal/sysinfo"
	"github.com/continuum-app/continuum-relay/internal/terminal"
	"github.com/continuum-app/continuum-relay/internal/wg"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// wgNetstack wraps a netstack.Net to create TCP listeners on the virtual network.
type wgNetstack struct {
	net *netstack.Net
}

func (w *wgNetstack) listenTCP(addr string) (net.Listener, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	return w.net.ListenTCPAddrPort(netip.AddrPortFrom(netip.MustParseAddr("10.100.0.1"), uint16(port)))
}

func main() {
	// ── CLI subcommand: peers ─────────────────────────────────────────────────
	if len(os.Args) > 1 && os.Args[1] == "peers" {
		runPeersCLI(os.Args[2:])
		return
	}

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

	// ── Signal handling ──────────────────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// ── WireGuard ────────────────────────────────────────────────────────────
	var wgNet *wgNetstack
	var wgServer *wg.Server // hoisted so peersMgr can use it as a LiveDevice
	if os.Getenv("CONTINUUM_WG_DISABLED") != "1" {
		wgConfPath := os.Getenv("CONTINUUM_WG_CONFIG")
		if wgConfPath == "" {
			wgConfPath = "/etc/wireguard/wg0.conf"
		}
		cfg, err := wg.ParseFile(wgConfPath)
		if err != nil {
			slog.Warn("WireGuard config not found — tunnel skipped", "path", wgConfPath, "err", err)
		} else {
			srv, err := wg.New(cfg)
			if err != nil {
				slog.Error("WireGuard init failed", "err", err)
				os.Exit(1)
			}
			if err := srv.Start(); err != nil {
				slog.Error("WireGuard start failed (needs root/CAP_NET_ADMIN)", "err", err)
				os.Exit(1)
			}
			defer srv.Close()
			wgServer = srv
			slog.Info("WireGuard tunnel active", "ip", wgServer.InterfaceIP())
			if wgServer.Net != nil {
				wgNet = &wgNetstack{net: wgServer.Net}
				slog.Info("using netstack virtual TCP/IP (macOS mode)")
			}
		}
	} else {
		slog.Info("WireGuard disabled (CONTINUUM_WG_DISABLED=1)")
	}

	// ── Terminal server (replaces ttyd) ──────────────────────────────────────
	termCmd := strings.Fields(os.Getenv("CONTINUUM_TERMINAL_CMD"))
	if len(termCmd) == 0 {
		// Use the detected shell command for the server's OS and user.
		// On macOS: /usr/bin/login -f <user> (avoids provenance restrictions)
		// On Linux: /bin/zsh -l or /bin/bash -l
		info := sysinfo.Detect()
		termCmd = info.ShellCommand
	}
	termAddr := os.Getenv("CONTINUUM_TERMINAL_ADDR")
	if termAddr == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			slog.Warn("could not parse relay addr for terminal addr derivation, using default", "addr", addr, "err", err)
			host = "10.100.0.1"
		}
		termAddr = net.JoinHostPort(host, "7681")
	}
	termServer := terminal.New(termAddr, token, termCmd)
	// Run PTY sessions as the detected user (not root).
	// Override with CONTINUUM_USER env var if needed.
	termUser := os.Getenv("CONTINUUM_USER")
	if termUser == "" {
		termUser = sysinfo.Detect().User
	}
	termServer.User = termUser
	if wgNet != nil {
		ln, err := wgNet.listenTCP(termAddr)
		if err != nil {
			slog.Error("netstack terminal listener failed", "err", err)
			os.Exit(1)
		}
		termServer.Listener = ln
	}
	go func() {
		if err := termServer.Run(ctx); err != nil {
			slog.Error("terminal server exited", "err", err)
		}
	}()

	// ── Peers manager ─────────────────────────────────────────────────────────
	var peersMgr *peers.Manager
	wgConfPath := os.Getenv("CONTINUUM_WG_CONFIG")
	if wgConfPath == "" {
		wgConfPath = "/etc/wireguard/wg0.conf"
	}
	publicIP := os.Getenv("CONTINUUM_PUBLIC_IP")
	if publicIP == "" {
		publicIP = discoverPublicIP()
	}
	if os.Getenv("CONTINUUM_WG_DISABLED") != "1" {
		// Wire the live wg device so peer add/remove takes immediate effect
		// on the running interface (not just the on-disk config).
		var liveDevice peers.LiveDevice
		if wgServer != nil {
			liveDevice = wgServer
		}
		peersMgr = peers.NewManager(wgConfPath, publicIP, token, liveDevice)
	}

	// ── Claude Code relay ─────────────────────────────────────────────────────
	apnsClient := buildAPNSClient()
	var relayListener net.Listener
	if wgNet != nil {
		var err error
		relayListener, err = wgNet.listenTCP(addr)
		if err != nil {
			slog.Error("netstack relay listener failed", "err", err)
			os.Exit(1)
		}
	}
	server := relay.NewServer(addr, token, apnsClient, peersMgr, relayListener)
	go func() {
		if err := server.Run(ctx); err != nil {
			slog.Error("relay server exited", "err", err)
		}
	}()

	// ── Wait for shutdown signal ──────────────────────────────────────────────
	<-sigCh
	slog.Info("received shutdown signal, shutting down…")
	cancel()

	time.Sleep(5 * time.Second)
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

// ── Peers CLI subcommand ─────────────────────────────────────────────────────

func runPeersCLI(args []string) {
	addr := os.Getenv("CONTINUUM_RELAY_ADDR")
	if addr == "" {
		addr = "10.100.0.1:7682"
	}
	token := os.Getenv("CONTINUUM_TOKEN")
	if token == "" {
		// Try reading from env file.
		if data, err := os.ReadFile("/etc/continuum/env"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "CONTINUUM_TOKEN=") {
					token = strings.TrimPrefix(line, "CONTINUUM_TOKEN=")
					break
				}
			}
		}
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: CONTINUUM_TOKEN not set and not found in /etc/continuum/env")
		os.Exit(1)
	}

	baseURL := "http://" + addr + "/api/peers"

	cmd := "help"
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "list", "ls":
		resp, err := apiRequest("GET", baseURL, token, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		var peerList []peers.Peer
		if err := json.Unmarshal(resp, &peerList); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing response: %v\n", err)
			os.Exit(1)
		}
		if len(peerList) == 0 {
			fmt.Println("No peers configured.")
			return
		}
		fmt.Printf("%-6s %-16s %-20s %s\n", "INDEX", "IP", "NAME", "PUBLIC KEY")
		for _, p := range peerList {
			pubShort := p.PublicKey
			if len(pubShort) > 20 {
				pubShort = pubShort[:20] + "…"
			}
			fmt.Printf("%-6d %-16s %-20s %s\n", p.Index, p.IP, p.Name, pubShort)
		}

	case "add":
		name := "device"
		if len(args) > 1 {
			name = args[1]
		}
		body, _ := json.Marshal(map[string]string{"name": name})
		resp, err := apiRequest("POST", baseURL, token, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		var result peers.AddResult
		if err := json.Unmarshal(resp, &result); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing response: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Added peer '%s' (IP: %s)\n\n", result.Peer.Name, result.Peer.IP)
		fmt.Println("Continuum QR payload (scan with the iOS app):")
		fmt.Println("")
		var pretty json.RawMessage = result.QRPayload
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
		fmt.Println("")
		fmt.Println("⚠️  Contains private key — do not share.")

	case "remove", "rm":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: continuum-relay peers remove <index>")
			os.Exit(1)
		}
		idx, err := strconv.Atoi(args[1])
		if err != nil || idx < 1 {
			fmt.Fprintln(os.Stderr, "error: index must be a positive integer")
			os.Exit(1)
		}
		_, err = apiRequest("DELETE", baseURL+"?index="+strconv.Itoa(idx), token, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Removed peer #%d.\n", idx)

	default:
		fmt.Println("Usage: continuum-relay peers <command>")
		fmt.Println("")
		fmt.Println("Commands:")
		fmt.Println("  list          Show all configured peers")
		fmt.Println("  add [name]    Add a new peer and show QR payload")
		fmt.Println("  remove <num>  Remove peer by index")
	}
}

func apiRequest(method, url, token string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed (is the relay running?): %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func discoverPublicIP() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://ifconfig.me")
	if err != nil {
		return "0.0.0.0"
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(data))
}
