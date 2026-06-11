package peers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/continuum-app/continuum-relay/internal/wg"
	"golang.org/x/crypto/curve25519"
)

const maxPeers = 10

// peerNameRegex enforces the same character class the iOS client validates
// against. Names land in `wg0.conf` as inline `[Peer]  # <name>` comments;
// anything outside this class — newlines especially — could inject extra
// directives (e.g. broadening AllowedIPs to 0.0.0.0/0).
var peerNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Peer represents a WireGuard peer with metadata.
type Peer struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	IP        string `json:"ip"`
}

// AddResult is returned after successfully adding a peer.
type AddResult struct {
	Peer      Peer            `json:"peer"`
	QRPayload json.RawMessage `json:"qrPayload"`
}

// LiveDevice is the subset of *wg.Server that peer management needs to
// keep the live tunnel interface in sync with wg0.conf changes. Production
// wires `*wg.Server` here; unit tests pass nil and skip live updates (the
// on-disk config is still updated and would take effect on a relay
// restart).
type LiveDevice interface {
	AddPeer(pubKeyB64, allowedCIDR string) error
	RemovePeer(pubKeyB64 string) error
}

// Manager handles WireGuard peer CRUD operations.
type Manager struct {
	mu         sync.Mutex
	confPath   string
	iface      string
	serverIP   string
	publicIP   string
	authToken  string
	serverPort int
	device     LiveDevice // nil in tests; non-nil in production
}

// NewManager creates a peer manager.
//
// `device` is the live WireGuard interface to keep in sync. In production
// pass the relay's `*wg.Server`; in unit tests pass nil and the manager
// will skip live updates.
func NewManager(confPath, publicIP, authToken string, device LiveDevice) *Manager {
	return &Manager{
		confPath:   confPath,
		iface:      "wg0",
		serverIP:   "10.100.0.1",
		publicIP:   publicIP,
		authToken:  authToken,
		serverPort: 51820,
		device:     device,
	}
}

// List returns all configured peers.
func (m *Manager) List() ([]Peer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := wg.ParseFile(m.confPath)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Pull the human-readable name from each `[Peer]  # name` comment in
	// wg0.conf. The wg config parser strips comments, so we do a separate
	// pass here. Fall back to a stable `device-N` label when a comment is
	// missing (older configs that pre-date appendPeer's comment block).
	names, _ := readPeerNames(m.confPath)

	peers := make([]Peer, 0, len(cfg.Peers))
	for i, p := range cfg.Peers {
		ip := ""
		if len(p.AllowedIPs) > 0 {
			ip = strings.TrimSuffix(p.AllowedIPs[0], "/32")
		}
		name := ""
		if i < len(names) {
			name = names[i]
		}
		if name == "" {
			name = fmt.Sprintf("device-%d", i+1)
		}
		peers = append(peers, Peer{
			Index:     i + 1,
			Name:      name,
			PublicKey: p.PublicKey,
			IP:        ip,
		})
	}
	return peers, nil
}

// Add generates a new peer, applies it to the live WireGuard interface,
// and persists it to wg0.conf so it survives a relay restart.
//
// The order is deliberate: the live update happens first, so a config-file
// failure rolls back the live state instead of leaving a "ghost peer" with
// access that the persisted config doesn't reflect.
func (m *Manager) Add(name string) (*AddResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Defence-in-depth: the iOS client validates name client-side, but a
	// buggy or malicious caller could otherwise inject newlines into wg0.conf
	// via the inline `[Peer]  # <name>` comment, broadening AllowedIPs and
	// granting tunnel access to arbitrary IPs.
	if !peerNameRegex.MatchString(name) {
		return nil, fmt.Errorf("invalid peer name: must match ^[a-zA-Z0-9_-]{1,64}$")
	}

	cfg, err := wg.ParseFile(m.confPath)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if len(cfg.Peers) >= maxPeers {
		return nil, fmt.Errorf("maximum peer count reached (%d)", maxPeers)
	}

	// Find next available IP.
	nextOctet := m.nextAvailableOctet(cfg)
	clientIP := fmt.Sprintf("%s.%d", "10.100.0", nextOctet)

	// Generate key pair.
	privKey, pubKey, err := generateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generate keys: %w", err)
	}

	// Apply to live WireGuard device first. If this fails the peer never
	// existed live, no rollback needed.
	if m.device != nil {
		if err := m.device.AddPeer(pubKey, clientIP+"/32"); err != nil {
			return nil, fmt.Errorf("apply peer to live interface: %w", err)
		}
	}

	// Persist to wg0.conf. If this fails the live device has a peer the
	// config doesn't, which would silently disappear on a relay restart —
	// roll back the live state to keep them in sync.
	if err := m.appendPeer(name, pubKey, clientIP); err != nil {
		if m.device != nil {
			if rmErr := m.device.RemovePeer(pubKey); rmErr != nil {
				slog.Error("rollback failed: peer is live but not in config",
					"err", rmErr, "pubkey", shortKey(pubKey))
			}
		}
		return nil, fmt.Errorf("write config: %w", err)
	}

	// Build QR payload.
	serverPubKey, err := m.serverPublicKey(cfg)
	if err != nil {
		return nil, fmt.Errorf("derive server pubkey: %w", err)
	}

	payload := map[string]any{
		"v":                  1,
		"serverName":         hostname(),
		"serverPublicIP":     m.publicIP,
		"wgServerEndpoint":   fmt.Sprintf("%s:%d", m.publicIP, m.serverPort),
		"wgServerPublicKey":  serverPubKey,
		"wgClientPrivateKey": privKey,
		"wgClientAddress":    clientIP + "/24",
		"wgDNS":              "1.1.1.1",
		"wgKeepalive":        25,
		"authToken":          m.authToken,
		"ttydPort":           7681,
		"relayPort":          7682,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal qr payload: %w", err)
	}

	slog.Info("peer added", "name", name, "ip", clientIP, "pubkey", shortKey(pubKey))

	return &AddResult{
		Peer: Peer{
			Index:     len(cfg.Peers) + 1,
			Name:      name,
			PublicKey: pubKey,
			IP:        clientIP,
		},
		QRPayload: payloadJSON,
	}, nil
}

// Remove revokes the peer at the given 1-based index. The live WireGuard
// interface is updated first — that is the actual access cut — and the
// wg0.conf rewrite follows so the peer doesn't reappear on restart.
//
// If the live revoke fails, the conf is left untouched and the call returns
// an error: better to be visibly wrong than silently fail to revoke.
func (m *Manager) Remove(index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cfg, err := wg.ParseFile(m.confPath)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	if index < 1 || index > len(cfg.Peers) {
		return fmt.Errorf("peer index %d out of range (1-%d)", index, len(cfg.Peers))
	}

	peer := cfg.Peers[index-1]

	// Live revoke first. If this errors, do not touch the conf — the peer
	// still has access and the operator needs to see the failure.
	if m.device != nil {
		if err := m.device.RemovePeer(peer.PublicKey); err != nil {
			return fmt.Errorf("revoke peer on live interface: %w", err)
		}
	}

	// Persist the removal so a relay restart doesn't re-add the peer.
	if err := m.rewriteWithout(index - 1); err != nil {
		// Live revoke already succeeded — peer is denied right now. We log
		// loudly and surface the error so the operator can manually clean
		// up the conf file before the next restart.
		slog.Error("conf rewrite failed after live revoke; peer is revoked but will reappear on restart",
			"err", err, "pubkey", shortKey(peer.PublicKey))
		return fmt.Errorf("rewrite config (peer revoked live but conf inconsistent): %w", err)
	}

	slog.Info("peer removed", "index", index, "pubkey", shortKey(peer.PublicKey))
	return nil
}

func (m *Manager) nextAvailableOctet(cfg *wg.Config) int {
	used := map[int]bool{1: true} // .1 is the server
	for _, p := range cfg.Peers {
		for _, cidr := range p.AllowedIPs {
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			parts := ip.To4()
			if parts != nil {
				used[int(parts[3])] = true
			}
		}
	}
	for i := 2; i < 255; i++ {
		if !used[i] {
			return i
		}
	}
	return 254
}

func (m *Manager) appendPeer(name, pubKey, clientIP string) error {
	f, err := os.OpenFile(m.confPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	block := fmt.Sprintf("\n[Peer]  # %s\nPublicKey = %s\nAllowedIPs = %s/32\n", name, pubKey, clientIP)
	_, err = f.WriteString(block)
	return err
}

func (m *Manager) rewriteWithout(removeIdx int) error {
	data, err := os.ReadFile(m.confPath)
	if err != nil {
		return err
	}

	// Split into sections by [Peer] headers.
	content := string(data)
	parts := splitSections(content)

	// parts[0] is [Interface] + anything before first [Peer].
	// parts[1..] are [Peer] blocks.
	if removeIdx+1 >= len(parts) {
		return fmt.Errorf("peer index out of range in file")
	}

	// Remove the peer block (index is 0-based into peer blocks, which start at parts[1]).
	peerIdx := removeIdx + 1
	parts = append(parts[:peerIdx], parts[peerIdx+1:]...)

	// Write atomically: a crash mid-write to confPath would leave a truncated
	// wg0.conf, and the relay fails to come up on restart (wg.ParseFile errors).
	// Write to a sibling temp file, then rename over the original.
	tmp := m.confPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(parts, "")), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, m.confPath)
}

// shortKey truncates a key for logging without panicking on short input.
// Keys from generateKeyPair are always 44 chars, but PublicKey values read
// back from a hand-edited wg0.conf may be shorter, and Remove() logs them.
func shortKey(k string) string {
	if len(k) > 20 {
		return k[:20] + "…"
	}
	return k
}

func (m *Manager) serverPublicKey(cfg *wg.Config) (string, error) {
	privBytes, err := base64.StdEncoding.DecodeString(cfg.Interface.PrivateKey)
	if err != nil {
		return "", err
	}
	if len(privBytes) != 32 {
		return "", fmt.Errorf("invalid private key length")
	}
	var pub, priv [32]byte
	copy(priv[:], privBytes)
	curve25519.ScalarBaseMult(&pub, &priv)
	return base64.StdEncoding.EncodeToString(pub[:]), nil
}

func generateKeyPair() (privB64, pubB64 string, err error) {
	var priv, pub [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return "", "", err
	}
	// Clamp private key per WireGuard spec.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	curve25519.ScalarBaseMult(&pub, &priv)
	return base64.StdEncoding.EncodeToString(priv[:]), base64.StdEncoding.EncodeToString(pub[:]), nil
}

// splitSections splits a WireGuard config into [Interface] header + [Peer] blocks.
func splitSections(content string) []string {
	var parts []string
	lines := strings.Split(content, "\n")
	var current strings.Builder
	first := true

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), "[peer]") && !first {
			parts = append(parts, current.String())
			current.Reset()
		}
		first = false
		current.WriteString(line)
		current.WriteByte('\n')
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// readPeerNames scans wg0.conf and returns the comment label associated with
// each `[Peer]` block, in order. The label is the text after the first '#'
// on the [Peer] header line — e.g. "[Peer]  # ipad" → "ipad". When a peer
// block has no inline comment, an empty string is returned for that index so
// callers can fall back to a default.
//
// We need this because the wg config parser strips comments before returning
// the parsed Config; the names live only in the source file.
func readPeerNames(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		// Section header? `[Peer]` (case-insensitive). Anything between the
		// closing bracket and an optional `#` is whitespace; everything after
		// the `#` is the comment.
		if !strings.HasPrefix(strings.ToLower(trimmed), "[peer]") {
			continue
		}

		// Strip the section header itself, then look for an inline comment.
		rest := strings.TrimSpace(trimmed[len("[Peer]"):])
		name := ""
		if idx := strings.Index(rest, "#"); idx >= 0 {
			name = strings.TrimSpace(rest[idx+1:])
		}
		names = append(names, name)
	}
	return names, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "continuum-server"
	}
	return h
}
