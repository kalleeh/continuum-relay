package main

import "testing"

// The relay binary is invoked both as a long-running server (by systemd, with
// no args or an explicit "serve") and as a CLI against the *already-running*
// relay ("peers", "status", "sessions"). The critical regression this guards
// against: an UNKNOWN argument must NEVER fall through to the server-boot path.
//
// Before this guard, `continuum-relay status` (a natural "check my sessions"
// command) was treated as "start the server", which tried to create the wg0
// TUN device the systemd service already owns and died with
// "device or resource busy" — read by users as "the relay crashed".
func TestResolveCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string // os.Args-style, including argv[0]
		want command
	}{
		{"no args boots server", []string{"continuum-relay"}, cmdServe},
		{"explicit serve", []string{"continuum-relay", "serve"}, cmdServe},
		{"peers", []string{"continuum-relay", "peers"}, cmdPeers},
		{"peers with subargs", []string{"continuum-relay", "peers", "add", "phone"}, cmdPeers},
		{"status", []string{"continuum-relay", "status"}, cmdStatus},
		{"sessions", []string{"continuum-relay", "sessions"}, cmdSessions},
		{"help", []string{"continuum-relay", "help"}, cmdHelp},
		{"-h", []string{"continuum-relay", "-h"}, cmdHelp},
		{"--help", []string{"continuum-relay", "--help"}, cmdHelp},
		// The regression: anything unrecognized must be an error, NOT a server boot.
		{"unknown does not boot server", []string{"continuum-relay", "bogus"}, cmdUnknown},
		{"typo of serve does not boot", []string{"continuum-relay", "serv"}, cmdUnknown},
		{"stray flag does not boot", []string{"continuum-relay", "--foo"}, cmdUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCommand(tc.args); got != tc.want {
				t.Errorf("resolveCommand(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
