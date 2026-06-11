package terminal

import "testing"

func TestStripSecrets(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/home/ubuntu",
		"CONTINUUM_TOKEN=supersecret",
		"CONTINUUM_RELAY=1",
		"TERM_PROGRAM=Continuum",
		"OLLAMA_API_KEY=ok-123",
		"BEDROCK_API_KEY=bk-123",
		"BEDROCK_MODEL=claude",
		"AWS_BEARER_TOKEN_BEDROCK=aws-123",
		"AWS_REGION=eu-west-1",
		"APNS_KEY_ID=ABC",
		"APNS_BUNDLE_ID=app.continuum",
	}
	out := stripSecrets(in)

	dropped := map[string]bool{
		"CONTINUUM_TOKEN=supersecret":      true,
		"OLLAMA_API_KEY=ok-123":            true,
		"BEDROCK_API_KEY=bk-123":           true,
		"BEDROCK_MODEL=claude":             true,
		"AWS_BEARER_TOKEN_BEDROCK=aws-123": true,
		"AWS_REGION=eu-west-1":             true,
		"APNS_KEY_ID=ABC":                  true,
		"APNS_BUNDLE_ID=app.continuum":     true,
	}
	kept := map[string]bool{
		"PATH=/usr/bin":          true,
		"HOME=/home/ubuntu":      true,
		"CONTINUUM_RELAY=1":      true,
		"TERM_PROGRAM=Continuum": true,
	}

	for _, e := range out {
		if dropped[e] {
			t.Errorf("secret leaked into PTY env: %q", e)
		}
		delete(kept, e)
	}
	if len(kept) != 0 {
		t.Errorf("non-secret entries were dropped: %v", kept)
	}

	// CONTINUUM_RELAY must survive even though it shares the CONTINUUM_ stem
	// with CONTINUUM_TOKEN — the filter matches the full "CONTINUUM_TOKEN="
	// prefix, not a bare "CONTINUUM_".
	var hasRelayMarker bool
	for _, e := range out {
		if e == "CONTINUUM_RELAY=1" {
			hasRelayMarker = true
		}
	}
	if !hasRelayMarker {
		t.Error("CONTINUUM_RELAY=1 marker was incorrectly stripped")
	}

	// Input slice must be untouched.
	if len(in) != 12 {
		t.Errorf("input slice mutated: len=%d, want 12", len(in))
	}
}
