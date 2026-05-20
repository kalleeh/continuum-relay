package peers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeConf writes the given content to a temp wg0.conf and returns its path.
// The Manager only uses the path itself; we never actually start a tunnel in
// these tests.
func writeConf(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wg0.conf")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	return path
}

// fakeLiveDevice records AddPeer / RemovePeer calls so tests can assert the
// expected live-update sequence happened (or didn't happen) around a conf
// rewrite.
type fakeLiveDevice struct {
	mu       sync.Mutex
	added    []string // public keys passed to AddPeer
	removed  []string // public keys passed to RemovePeer
	addErr   error    // returned from AddPeer if non-nil
	rmErr    error    // returned from RemovePeer if non-nil
	allowed  []string // CIDR passed alongside last AddPeer
}

func (f *fakeLiveDevice) AddPeer(pubKey, allowedCIDR string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, pubKey)
	f.allowed = append(f.allowed, allowedCIDR)
	return nil
}

func (f *fakeLiveDevice) RemovePeer(pubKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rmErr != nil {
		return f.rmErr
	}
	f.removed = append(f.removed, pubKey)
	return nil
}

const interfaceBlock = `[Interface]
PrivateKey = IDCRfOZL/4OGHjW3TiWN3bgwuJdV/ntbXxVAZkxaplQ=
Address = 10.100.0.1/24
ListenPort = 51820
`

// Each test inserts peers into a fresh wg0.conf, constructs a Manager, and
// asserts that List() returns the comment-derived names rather than the old
// device-N placeholder.

func TestList_NamesComeFromComments(t *testing.T) {
	conf := interfaceBlock + `
[Peer]  # karls-iphone
PublicKey = oiRIV5cc6M2I1KFgbXgZUPVepTn1dypOBy4TWBfnRWw=
AllowedIPs = 10.100.0.2/32

[Peer]  # ipad
PublicKey = ApDvjf/jZ/NiBvLyupp4bdZExhU0Q58oAvDFXJ4eFXA=
AllowedIPs = 10.100.0.3/32
`
	mgr := NewManager(writeConf(t, conf), "1.2.3.4", "deadbeef", nil)

	peers, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("want 2 peers, got %d", len(peers))
	}
	if peers[0].Name != "karls-iphone" {
		t.Errorf("peer[0].Name = %q, want karls-iphone", peers[0].Name)
	}
	if peers[1].Name != "ipad" {
		t.Errorf("peer[1].Name = %q, want ipad", peers[1].Name)
	}
	if peers[0].IP != "10.100.0.2" {
		t.Errorf("peer[0].IP = %q, want 10.100.0.2", peers[0].IP)
	}
	if peers[0].Index != 1 || peers[1].Index != 2 {
		t.Errorf("indices = %d,%d, want 1,2", peers[0].Index, peers[1].Index)
	}
}

func TestList_FallsBackWhenCommentMissing(t *testing.T) {
	conf := interfaceBlock + `
[Peer]
PublicKey = oiRIV5cc6M2I1KFgbXgZUPVepTn1dypOBy4TWBfnRWw=
AllowedIPs = 10.100.0.2/32

[Peer]  # ipad
PublicKey = ApDvjf/jZ/NiBvLyupp4bdZExhU0Q58oAvDFXJ4eFXA=
AllowedIPs = 10.100.0.3/32
`
	mgr := NewManager(writeConf(t, conf), "1.2.3.4", "deadbeef", nil)

	peers, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if peers[0].Name != "device-1" {
		t.Errorf("peer[0].Name = %q, want device-1 (fallback)", peers[0].Name)
	}
	if peers[1].Name != "ipad" {
		t.Errorf("peer[1].Name = %q, want ipad", peers[1].Name)
	}
}

func TestList_HandlesWeirdSpacing(t *testing.T) {
	conf := interfaceBlock + `
[Peer]#nospaces
PublicKey = oiRIV5cc6M2I1KFgbXgZUPVepTn1dypOBy4TWBfnRWw=
AllowedIPs = 10.100.0.2/32

[Peer]   #   lots-of-spaces
PublicKey = ApDvjf/jZ/NiBvLyupp4bdZExhU0Q58oAvDFXJ4eFXA=
AllowedIPs = 10.100.0.3/32

[peer] # lowercase-header
PublicKey = QRIdBPkHad9zkP3KahFo0HG5ExBtDhZKvglgb354pmg=
AllowedIPs = 10.100.0.4/32
`
	mgr := NewManager(writeConf(t, conf), "1.2.3.4", "deadbeef", nil)

	peers, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(peers) != 3 {
		t.Fatalf("want 3 peers, got %d", len(peers))
	}
	wantNames := []string{"nospaces", "lots-of-spaces", "lowercase-header"}
	for i, want := range wantNames {
		if peers[i].Name != want {
			t.Errorf("peer[%d].Name = %q, want %q", i, peers[i].Name, want)
		}
	}
}

// Round-trip test: Add() writes "[Peer]  # name" and List() reads it back.
func TestAddThenList_PreservesName(t *testing.T) {
	conf := interfaceBlock
	path := writeConf(t, conf)
	dev := &fakeLiveDevice{}
	mgr := NewManager(path, "1.2.3.4", "deadbeef", dev)

	if _, err := mgr.Add("test-laptop"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	peers, err := mgr.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("want 1 peer after Add, got %d", len(peers))
	}
	if peers[0].Name != "test-laptop" {
		t.Errorf("peer[0].Name = %q, want test-laptop", peers[0].Name)
	}
	if len(dev.added) != 1 {
		t.Errorf("expected exactly one AddPeer call to the live device, got %d", len(dev.added))
	}
	if len(dev.allowed) != 1 || dev.allowed[0] != "10.100.0.2/32" {
		t.Errorf("allowed CIDR = %v, want [10.100.0.2/32]", dev.allowed)
	}
}

// Defence-in-depth: Add() must reject names that don't match the safe charset.
// A name with a newline could inject extra config directives (e.g. broadening
// AllowedIPs) into wg0.conf via the inline `[Peer]  # <name>` comment.
func TestAdd_RejectsInjectionInName(t *testing.T) {
	conf := interfaceBlock
	path := writeConf(t, conf)
	dev := &fakeLiveDevice{}
	mgr := NewManager(path, "1.2.3.4", "deadbeef", dev)

	bad := []string{
		"ipad\nAllowedIPs = 0.0.0.0/0",
		"a b", // space not allowed
		"",
		strings.Repeat("a", 65),
		"name#with-hash",
		"name/with/slash",
		"emoji-🎉",
	}
	for _, name := range bad {
		_, err := mgr.Add(name)
		if err == nil {
			t.Errorf("Add(%q) should have been rejected", name)
		}
	}
	if len(dev.added) != 0 {
		t.Errorf("rejected names should not have called AddPeer; got %d calls", len(dev.added))
	}
}

// Critical: Remove() must call the live RemovePeer FIRST, before rewriting
// the conf. That ordering is what makes revocation actually take effect
// immediately (rather than only on the next relay restart, the long-standing
// bug this commit fixes).
func TestRemove_RevokesLiveBeforeConf(t *testing.T) {
	conf := interfaceBlock + `
[Peer]  # to-revoke
PublicKey = oiRIV5cc6M2I1KFgbXgZUPVepTn1dypOBy4TWBfnRWw=
AllowedIPs = 10.100.0.2/32
`
	path := writeConf(t, conf)
	dev := &fakeLiveDevice{}
	mgr := NewManager(path, "1.2.3.4", "deadbeef", dev)

	if err := mgr.Remove(1); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Live revocation must have happened, with the right pubkey.
	if len(dev.removed) != 1 {
		t.Fatalf("expected 1 RemovePeer call, got %d", len(dev.removed))
	}
	if dev.removed[0] != "oiRIV5cc6M2I1KFgbXgZUPVepTn1dypOBy4TWBfnRWw=" {
		t.Errorf("removed pubkey = %q, want oiRIV...", dev.removed[0])
	}

	// Conf must no longer contain the peer.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "to-revoke") {
		t.Errorf("conf still contains revoked peer:\n%s", raw)
	}
}

// If the live revoke fails, the conf must NOT be rewritten — otherwise the
// caller would think the peer was revoked while the live tunnel still has
// the peer's key.
func TestRemove_FailsLoudIfLiveRevokeFails(t *testing.T) {
	conf := interfaceBlock + `
[Peer]  # to-revoke
PublicKey = oiRIV5cc6M2I1KFgbXgZUPVepTn1dypOBy4TWBfnRWw=
AllowedIPs = 10.100.0.2/32
`
	path := writeConf(t, conf)
	dev := &fakeLiveDevice{rmErr: errors.New("uapi unavailable")}
	mgr := NewManager(path, "1.2.3.4", "deadbeef", dev)

	if err := mgr.Remove(1); err == nil {
		t.Fatalf("Remove with failing live device should have errored")
	}

	// Conf must still contain the peer (no rewrite happened).
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "to-revoke") {
		t.Errorf("conf was rewritten despite live revoke failure:\n%s", raw)
	}
}

// If live Add fails, conf must NOT be appended (rolling back to a clean
// state rather than persisting a peer that the live device never accepted).
func TestAdd_RollsBackOnLiveFailure(t *testing.T) {
	path := writeConf(t, interfaceBlock)
	dev := &fakeLiveDevice{addErr: errors.New("uapi unavailable")}
	mgr := NewManager(path, "1.2.3.4", "deadbeef", dev)

	if _, err := mgr.Add("test"); err == nil {
		t.Fatalf("Add with failing live device should have errored")
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "[Peer]") {
		t.Errorf("conf was written despite live Add failure:\n%s", raw)
	}
}
