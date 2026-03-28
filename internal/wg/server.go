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
	"os/exec"
	"runtime"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

// Server manages an embedded WireGuard tunnel.
type Server struct {
	cfg    *Config
	device *device.Device
	tunDev tun.Device
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
	tunName := "wg0"
	if runtime.GOOS == "darwin" {
		tunName = "utun"
	}

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

	// 2. Create WireGuard device.
	logger := device.NewLogger(device.LogLevelError, "[wg] ")
	wgDev := device.NewDevice(tunDevice, conn.NewDefaultBind(), logger)
	s.device = wgDev

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

// buildUAPIConfig constructs the UAPI text protocol string for IpcSet.
// Format: https://www.wireguard.com/xplatform/#configuration-protocol
func (s *Server) buildUAPIConfig() (string, error) {
	var b strings.Builder

	privHex, err := b64ToHex(s.cfg.Interface.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("private_key base64 decode: %w", err)
	}

	b.WriteString("set=1\n")
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
	return hex.EncodeToString(raw), nil
}
