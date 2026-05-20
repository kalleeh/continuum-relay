package wg

// REQUIRED go.mod dependencies (DO NOT EDIT go.mod — list here for integration step):
// require (
//     golang.zx2c4.com/wireguard v0.0.0-20231211153847-12269c276173
// )

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

var tunNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,16}$`)

// Server manages an embedded WireGuard tunnel.
type Server struct {
	cfg    *Config
	device *device.Device
	tunDev tun.Device
	// Net is the virtual TCP/IP stack (netstack mode only, used on macOS).
	// When non-nil, the relay should use Net.ListenTCP instead of binding to the TUN IP.
	Net *netstack.Net
}

// New creates a WireGuard server from parsed config. Does not start the tunnel yet.
func New(cfg *Config) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("wg: config must not be nil")
	}
	if cfg.Interface.PrivateKey == "" {
		return nil, fmt.Errorf("wg: Interface.PrivateKey is required")
	}
	if cfg.Interface.Address == "" {
		return nil, fmt.Errorf("wg: Interface.Address is required")
	}
	return &Server{cfg: cfg}, nil
}

// Start brings up the WireGuard tunnel:
//  1. Creates TUN device
//  2. Configures WireGuard via IpcSet (UAPI text protocol)
//  3. Sets interface IP address via OS command
//  4. Calls device.Up()
//
// Returns when tunnel is active.
func (s *Server) Start() error {
	// On macOS, use netstack (virtual TCP/IP stack in userspace) instead of a
	// real TUN device. macOS cannot route packets destined for a TUN interface's
	// own IP back to the local TCP stack — they get consumed by the userspace
	// WireGuard and never reach listeners. Netstack bypasses this entirely.
	if runtime.GOOS == "darwin" {
		return s.startNetstack()
	}
	return s.startTUN()
}

func (s *Server) startNetstack() error {
	ip, _, err := net.ParseCIDR(s.cfg.Interface.Address)
	if err != nil {
		return fmt.Errorf("wg: parse interface address: %w", err)
	}
	addr, ok := netip.AddrFromSlice(ip.To4())
	if !ok {
		return fmt.Errorf("wg: invalid IPv4 address: %s", ip)
	}

	tunDevice, tnet, err := netstack.CreateNetTUN(
		[]netip.Addr{addr},
		[]netip.Addr{netip.MustParseAddr("1.1.1.1")},
		1420,
	)
	if err != nil {
		return fmt.Errorf("wg: create netstack TUN: %w", err)
	}
	s.tunDev = tunDevice
	s.Net = tnet

	logger := device.NewLogger(device.LogLevelError, "[wg] ")
	wgDev := device.NewDevice(tunDevice, conn.NewDefaultBind(), logger)

	uapiCfg, err := s.buildUAPIConfig()
	if err != nil {
		wgDev.Close()
		return fmt.Errorf("wg: build UAPI config: %w", err)
	}
	if err := wgDev.IpcSet(uapiCfg); err != nil {
		wgDev.Close()
		return fmt.Errorf("wg: IpcSet: %w", err)
	}
	if err := wgDev.Up(); err != nil {
		wgDev.Close()
		return fmt.Errorf("wg: device.Up: %w", err)
	}

	s.device = wgDev
	return nil
}

func (s *Server) startTUN() error {
	tunName := "wg0"

	// 1. Create TUN device.
	tunDevice, err := tun.CreateTUN(tunName, device.DefaultMTU)
	if err != nil {
		return fmt.Errorf("wg: create TUN device: %w", err)
	}
	s.tunDev = tunDevice

	// Get the actual OS-assigned name (important on macOS where "utun" becomes "utunN").
	actualName, err := tunDevice.Name()
	if err != nil {
		tunDevice.Close()
		return fmt.Errorf("wg: get TUN device name: %w", err)
	}
	// Validate the name is a safe interface name (alphanumeric, hyphens, underscores only).
	if !tunNameRe.MatchString(actualName) {
		tunDevice.Close()
		return fmt.Errorf("wg: unexpected TUN device name: %q", actualName)
	}

	// 2. Create WireGuard device.
	logger := device.NewLogger(device.LogLevelError, "[wg] ")
	wgDev := device.NewDevice(tunDevice, conn.NewDefaultBind(), logger)

	// 3. Configure via UAPI.
	uapiCfg, err := s.buildUAPIConfig()
	if err != nil {
		wgDev.Close()
		return fmt.Errorf("wg: build UAPI config: %w", err)
	}
	if err := wgDev.IpcSet(uapiCfg); err != nil {
		wgDev.Close()
		return fmt.Errorf("wg: IpcSet: %w", err)
	}

	// 4. Bring device up.
	if err := wgDev.Up(); err != nil {
		wgDev.Close()
		return fmt.Errorf("wg: device.Up: %w", err)
	}

	// 5. Configure the network interface (assign IP, set up link).
	if err := s.configureInterface(actualName); err != nil {
		wgDev.Close()
		return fmt.Errorf("wg: configure interface %q: %w", actualName, err)
	}

	// Only assign to s.device after all setup succeeds, so s.Close() cannot
	// double-close a device that was already closed on an error path above.
	s.device = wgDev

	return nil
}

// Close tears down the tunnel.
func (s *Server) Close() {
	if s.device != nil {
		s.device.Close()
		s.device = nil
	}
	// tunDev is owned and closed by device.Close(); nilling to prevent double-close.
	s.tunDev = nil
}

// InterfaceIP returns the server's WireGuard IP without the prefix length
// (e.g. "10.100.0.1" from "10.100.0.1/24") so callers can bind services to it.
func (s *Server) InterfaceIP() string {
	ip, _, err := net.ParseCIDR(s.cfg.Interface.Address)
	if err != nil {
		// Address validated in New; this should never happen.
		return s.cfg.Interface.Address
	}
	return ip.String()
}

// AddPeer pushes a new peer to the live WireGuard device via the in-process
// UAPI. Allowed IPs are appended; existing peer config (if the public key is
// already known to the device) is preserved.
//
// Production callers should pair this with an append to wg0.conf so the
// peer survives a relay restart. See peers.Manager.Add.
func (s *Server) AddPeer(pubKeyB64, allowedCIDR string) error {
	if s.device == nil {
		return fmt.Errorf("wg: device not running")
	}
	if err := validateNoNewlines("public_key", pubKeyB64); err != nil {
		return err
	}
	if err := validateNoNewlines("allowed_ip", allowedCIDR); err != nil {
		return err
	}
	pubHex, err := b64ToHex(pubKeyB64)
	if err != nil {
		return fmt.Errorf("wg: public_key decode: %w", err)
	}
	cmd := fmt.Sprintf("public_key=%s\nallowed_ip=%s\n\n", pubHex, allowedCIDR)
	return s.device.IpcSet(cmd)
}

// RemovePeer removes a peer from the live WireGuard device via the in-process
// UAPI. Subsequent handshake attempts from that peer's public key are
// rejected immediately — this is the actual access-revocation primitive.
//
// Production callers should pair this with a wg0.conf rewrite so the peer
// stays revoked after a relay restart. See peers.Manager.Remove.
func (s *Server) RemovePeer(pubKeyB64 string) error {
	if s.device == nil {
		return fmt.Errorf("wg: device not running")
	}
	if err := validateNoNewlines("public_key", pubKeyB64); err != nil {
		return err
	}
	pubHex, err := b64ToHex(pubKeyB64)
	if err != nil {
		return fmt.Errorf("wg: public_key decode: %w", err)
	}
	cmd := fmt.Sprintf("public_key=%s\nremove=true\n\n", pubHex)
	return s.device.IpcSet(cmd)
}

func validateNoNewlines(field, value string) error {
	if strings.ContainsAny(value, "\n\r") {
		return fmt.Errorf("wg config field %q contains invalid characters", field)
	}
	return nil
}

// buildUAPIConfig constructs the UAPI text protocol string for IpcSet.
// Format: https://www.wireguard.com/xplatform/#configuration-protocol
func (s *Server) buildUAPIConfig() (string, error) {
	if err := validateNoNewlines("PrivateKey", s.cfg.Interface.PrivateKey); err != nil {
		return "", err
	}
	if err := validateNoNewlines("Address", s.cfg.Interface.Address); err != nil {
		return "", err
	}
	for _, peer := range s.cfg.Peers {
		if err := validateNoNewlines("PublicKey", peer.PublicKey); err != nil {
			return "", err
		}
		if err := validateNoNewlines("Endpoint", peer.Endpoint); err != nil {
			return "", err
		}
		if err := validateNoNewlines("PreSharedKey", peer.PreSharedKey); err != nil {
			return "", err
		}
	}

	var b strings.Builder

	privHex, err := b64ToHex(s.cfg.Interface.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("private_key base64 decode: %w", err)
	}

	// device.IpcSet takes the configuration body only — the "set=1" verb
	// is consumed by the UAPI socket layer and must NOT be included here.
	fmt.Fprintf(&b, "private_key=%s\n", privHex)
	if s.cfg.Interface.ListenPort > 0 {
		fmt.Fprintf(&b, "listen_port=%d\n", s.cfg.Interface.ListenPort)
	}

	for _, peer := range s.cfg.Peers {
		pubHex, err := b64ToHex(peer.PublicKey)
		if err != nil {
			return "", fmt.Errorf("peer public_key base64 decode: %w", err)
		}
		fmt.Fprintf(&b, "public_key=%s\n", pubHex)
		if peer.PreSharedKey != "" {
			pskHex, err := b64ToHex(peer.PreSharedKey)
			if err != nil {
				return "", fmt.Errorf("peer preshared_key base64 decode: %w", err)
			}
			fmt.Fprintf(&b, "preshared_key=%s\n", pskHex)
		}
		b.WriteString("replace_allowed_ips=true\n")
		for _, cidr := range peer.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", cidr)
		}
		if peer.Endpoint != "" {
			fmt.Fprintf(&b, "endpoint=%s\n", peer.Endpoint)
		}
		if peer.PersistentKeepalive > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", peer.PersistentKeepalive)
		}
	}

	b.WriteString("\n") // UAPI requires blank line to terminate the set command
	return b.String(), nil
}

// configureInterface assigns the IP address to the TUN interface using
// OS-native commands. Must be called after device.Up().
func (s *Server) configureInterface(tunName string) error {
	switch runtime.GOOS {
	case "linux":
		if err := exec.Command("ip", "addr", "add", s.cfg.Interface.Address, "dev", tunName).Run(); err != nil {
			return fmt.Errorf("ip addr add: %w", err)
		}
		if err := exec.Command("ip", "link", "set", "up", "dev", tunName).Run(); err != nil {
			return fmt.Errorf("ip link set up: %w", err)
		}
		return nil

	case "darwin":
		ip, _, err := net.ParseCIDR(s.cfg.Interface.Address)
		if err != nil {
			return fmt.Errorf("parse interface address %q: %w", s.cfg.Interface.Address, err)
		}
		// ifconfig <iface> <local-ip> <dest-ip> netmask <mask> up
		// For a point-to-point style setup, local == dest is conventional.
		if err := exec.Command(
			"ifconfig", tunName,
			ip.String(), ip.String(),
			"netmask", "255.255.255.0",
			"up",
		).Run(); err != nil {
			return fmt.Errorf("ifconfig: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// b64ToHex converts a standard base64 string to its lowercase hex representation.
// WireGuard config files use base64 keys; the UAPI protocol requires hex.
func b64ToHex(b64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("WireGuard key must be 32 bytes, got %d", len(raw))
	}
	return hex.EncodeToString(raw), nil
}
