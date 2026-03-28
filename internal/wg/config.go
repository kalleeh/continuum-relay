package wg

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const maxWGPeers = 100

// Config holds the full parsed WireGuard configuration.
type Config struct {
	Interface InterfaceConfig
	Peers     []PeerConfig
}

// InterfaceConfig holds the [Interface] section fields.
type InterfaceConfig struct {
	PrivateKey string // base64-encoded 32-byte key
	Address    string // e.g. "10.100.0.1/24"
	ListenPort int
}

// PeerConfig holds a single [Peer] section.
type PeerConfig struct {
	PublicKey           string   // base64-encoded 32-byte key
	AllowedIPs          []string // e.g. ["10.100.0.2/32"]
	Endpoint            string   // optional, e.g. "1.2.3.4:51820"
	PersistentKeepalive int
}

// ParseFile reads and parses a WireGuard config file.
func ParseFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wg: read config file %q: %w", path, err)
	}
	return ParseString(string(data))
}

// ParseString parses WireGuard config from a string (useful for testing).
func ParseString(s string) (*Config, error) {
	cfg := &Config{}

	// section tracks which section we're currently parsing.
	// "": before any section header
	// "interface": inside [Interface]
	// "peer": inside a [Peer] block
	section := ""
	var currentPeer *PeerConfig

	scanner := bufio.NewScanner(strings.NewReader(s))
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// Strip inline comment: everything after the first unquoted '#'.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)

		// Skip blank lines.
		if line == "" {
			continue
		}

		// Section header?
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			header := strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch header {
			case "interface":
				// Commit any pending peer before switching sections.
				if currentPeer != nil {
					cfg.Peers = append(cfg.Peers, *currentPeer)
					currentPeer = nil
				}
				section = "interface"
			case "peer":
				// Commit any pending peer.
				if currentPeer != nil {
					cfg.Peers = append(cfg.Peers, *currentPeer)
				}
				currentPeer = &PeerConfig{}
				section = "peer"
			default:
				// Unknown section — ignore but stop accumulating into the current one.
				if currentPeer != nil {
					cfg.Peers = append(cfg.Peers, *currentPeer)
					currentPeer = nil
				}
				section = "unknown"
			}
			continue
		}

		// Key = Value line.
		eqIdx := strings.Index(line, "=")
		if eqIdx < 0 {
			// Malformed line — silently skip.
			continue
		}
		key := strings.TrimSpace(line[:eqIdx])
		value := strings.TrimSpace(line[eqIdx+1:])

		switch section {
		case "interface":
			if err := applyInterfaceKey(&cfg.Interface, key, value); err != nil {
				return nil, fmt.Errorf("wg: line %d: %w", lineNum, err)
			}
		case "peer":
			if currentPeer == nil {
				currentPeer = &PeerConfig{}
			}
			if err := applyPeerKey(currentPeer, key, value); err != nil {
				return nil, fmt.Errorf("wg: line %d: %w", lineNum, err)
			}
		}
		// "unknown" and "" sections: silently ignore.
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("wg: scan error: %w", err)
	}

	// Commit the last peer if any.
	if currentPeer != nil {
		cfg.Peers = append(cfg.Peers, *currentPeer)
	}

	if len(cfg.Peers) > maxWGPeers {
		return nil, fmt.Errorf("wg config: too many peers (%d, max %d)", len(cfg.Peers), maxWGPeers)
	}

	return cfg, nil
}

func applyInterfaceKey(iface *InterfaceConfig, key, value string) error {
	switch strings.ToLower(key) {
	case "privatekey":
		iface.PrivateKey = value
	case "address":
		iface.Address = value
	case "listenport":
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("invalid ListenPort: %w", err)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("ListenPort must be 1-65535, got %d", port)
		}
		iface.ListenPort = port
	// Unknown keys (DNS, MTU, PostUp, etc.) are silently ignored.
	}
	return nil
}

func applyPeerKey(peer *PeerConfig, key, value string) error {
	switch strings.ToLower(key) {
	case "publickey":
		peer.PublicKey = value
	case "allowedips":
		// Value may be comma-separated: "10.0.0.2/32, 10.0.0.3/32"
		for _, cidr := range strings.Split(value, ",") {
			cidr = strings.TrimSpace(cidr)
			if cidr != "" {
				peer.AllowedIPs = append(peer.AllowedIPs, cidr)
			}
		}
	case "endpoint":
		peer.Endpoint = value
	case "persistentkeepalive":
		ka, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("invalid PersistentKeepalive %q: %w", value, err)
		}
		peer.PersistentKeepalive = ka
	// Unknown keys (PreSharedKey, etc.) are silently ignored.
	}
	return nil
}
