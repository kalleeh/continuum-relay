package wg

import (
	"testing"
)

const exampleConfig = `
[Interface]
PrivateKey = abc123base64=
Address = 10.100.0.1/24
ListenPort = 51820

[Peer]  # Device 1
PublicKey = xyz789base64=
AllowedIPs = 10.100.0.2/32
PersistentKeepalive = 25

[Peer]
PublicKey = def456base64=
AllowedIPs = 10.100.0.3/32
`

func TestParseString_BasicExample(t *testing.T) {
	cfg, err := ParseString(exampleConfig)
	if err != nil {
		t.Fatalf("ParseString returned unexpected error: %v", err)
	}

	// Interface checks.
	if got, want := cfg.Interface.PrivateKey, "abc123base64="; got != want {
		t.Errorf("Interface.PrivateKey = %q, want %q", got, want)
	}
	if got, want := cfg.Interface.Address, "10.100.0.1/24"; got != want {
		t.Errorf("Interface.Address = %q, want %q", got, want)
	}
	if got, want := cfg.Interface.ListenPort, 51820; got != want {
		t.Errorf("Interface.ListenPort = %d, want %d", got, want)
	}

	// Peer count.
	if got, want := len(cfg.Peers), 2; got != want {
		t.Fatalf("len(Peers) = %d, want %d", got, want)
	}

	// Peer 0.
	p0 := cfg.Peers[0]
	if got, want := p0.PublicKey, "xyz789base64="; got != want {
		t.Errorf("Peers[0].PublicKey = %q, want %q", got, want)
	}
	if got, want := len(p0.AllowedIPs), 1; got != want {
		t.Fatalf("len(Peers[0].AllowedIPs) = %d, want %d", got, want)
	}
	if got, want := p0.AllowedIPs[0], "10.100.0.2/32"; got != want {
		t.Errorf("Peers[0].AllowedIPs[0] = %q, want %q", got, want)
	}
	if got, want := p0.PersistentKeepalive, 25; got != want {
		t.Errorf("Peers[0].PersistentKeepalive = %d, want %d", got, want)
	}

	// Peer 1.
	p1 := cfg.Peers[1]
	if got, want := p1.PublicKey, "def456base64="; got != want {
		t.Errorf("Peers[1].PublicKey = %q, want %q", got, want)
	}
	if got, want := len(p1.AllowedIPs), 1; got != want {
		t.Fatalf("len(Peers[1].AllowedIPs) = %d, want %d", got, want)
	}
	if got, want := p1.AllowedIPs[0], "10.100.0.3/32"; got != want {
		t.Errorf("Peers[1].AllowedIPs[0] = %q, want %q", got, want)
	}
	if got, want := p1.PersistentKeepalive, 0; got != want {
		t.Errorf("Peers[1].PersistentKeepalive = %d, want %d", got, want)
	}
}

func TestParseString_InlineComment(t *testing.T) {
	input := `
[Interface]
PrivateKey = key1= # this is a comment
Address = 10.0.0.1/24
ListenPort = 51820
`
	cfg, err := ParseString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The value must be stripped of the comment but trailing spaces should be trimmed.
	if got, want := cfg.Interface.PrivateKey, "key1="; got != want {
		t.Errorf("PrivateKey = %q, want %q", got, want)
	}
}

func TestParseString_MultipleAllowedIPs_CommaSeparated(t *testing.T) {
	input := `
[Interface]
PrivateKey = k=
Address = 10.0.0.1/24
ListenPort = 51820

[Peer]
PublicKey = p=
AllowedIPs = 10.0.0.2/32, 10.0.0.3/32
`
	cfg, err := ParseString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(cfg.Peers))
	}
	if got, want := len(cfg.Peers[0].AllowedIPs), 2; got != want {
		t.Errorf("len(AllowedIPs) = %d, want %d", got, want)
	}
}

func TestParseString_MultipleAllowedIPs_MultiLine(t *testing.T) {
	input := `
[Interface]
PrivateKey = k=
Address = 10.0.0.1/24
ListenPort = 51820

[Peer]
PublicKey = p=
AllowedIPs = 10.0.0.2/32
AllowedIPs = 10.0.0.3/32
`
	cfg, err := ParseString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(cfg.Peers))
	}
	if got, want := len(cfg.Peers[0].AllowedIPs), 2; got != want {
		t.Errorf("len(AllowedIPs) = %d, want %d", got, want)
	}
}

func TestParseString_CaseInsensitiveSectionHeaders(t *testing.T) {
	input := `
[INTERFACE]
PrivateKey = k=
Address = 10.0.0.1/24
ListenPort = 51820

[PEER]
PublicKey = p=
AllowedIPs = 10.0.0.2/32
`
	cfg, err := ParseString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Interface.ListenPort != 51820 {
		t.Errorf("ListenPort = %d, want 51820", cfg.Interface.ListenPort)
	}
	if len(cfg.Peers) != 1 {
		t.Errorf("expected 1 peer, got %d", len(cfg.Peers))
	}
}

func TestParseString_UnknownKeysIgnored(t *testing.T) {
	input := `
[Interface]
PrivateKey = k=
Address = 10.0.0.1/24
ListenPort = 51820
DNS = 1.1.1.1
MTU = 1420

[Peer]
PublicKey = p=
AllowedIPs = 0.0.0.0/0
PreSharedKey = sharedkey=
`
	cfg, err := ParseString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Interface.ListenPort != 51820 {
		t.Errorf("ListenPort = %d, want 51820", cfg.Interface.ListenPort)
	}
	if len(cfg.Peers) != 1 {
		t.Errorf("expected 1 peer, got %d", len(cfg.Peers))
	}
}

func TestParseString_EmptyInput(t *testing.T) {
	cfg, err := ParseString("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Interface.PrivateKey != "" {
		t.Errorf("expected empty PrivateKey, got %q", cfg.Interface.PrivateKey)
	}
	if len(cfg.Peers) != 0 {
		t.Errorf("expected 0 peers, got %d", len(cfg.Peers))
	}
}

func TestParseString_InvalidListenPort(t *testing.T) {
	input := `
[Interface]
PrivateKey = k=
Address = 10.0.0.1/24
ListenPort = notanumber
`
	_, err := ParseString(input)
	if err == nil {
		t.Fatal("expected error for invalid ListenPort, got nil")
	}
}

func TestParseString_PeerEndpoint(t *testing.T) {
	input := `
[Interface]
PrivateKey = k=
Address = 10.0.0.1/24
ListenPort = 51820

[Peer]
PublicKey = p=
AllowedIPs = 10.0.0.2/32
Endpoint = 1.2.3.4:51820
`
	cfg, err := ParseString(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := cfg.Peers[0].Endpoint, "1.2.3.4:51820"; got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
}
