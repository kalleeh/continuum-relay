package relay

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWavFile(t *testing.T) {
	pcm := make([]byte, 3200)
	w := wavFile(pcm, 16000)
	if len(w) != 44+3200 || string(w[0:4]) != "RIFF" || string(w[8:12]) != "WAVE" || string(w[36:40]) != "data" {
		t.Fatalf("bad wav header: %q", w[:44])
	}
	if w[24] != 0x80 || w[25] != 0x3e { // 16000 = 0x3e80, little endian
		t.Errorf("sample rate bytes %x %x", w[24], w[25])
	}
}

func TestCleanTranscript(t *testing.T) {
	for in, want := range map[string]string{
		" fix the failing test\n":                "fix the failing test",
		"[BLANK_AUDIO]":                          "",
		" hello (music) world \n and more [x]\n": "hello world and more",
		"♪ la la ♪":                              "la la",
	} {
		if got := cleanTranscript(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestAudioInbox(t *testing.T) {
	var in audioInbox
	in.begin("7", 16000)
	chunk := base64.StdEncoding.EncodeToString(make([]byte, 100))
	if !in.add("7", chunk) || !in.add("7", chunk) || len(in.pcm) != 200 {
		t.Fatalf("chunks not collected: %d", len(in.pcm))
	}
	if in.add("8", chunk) || in.add("7", "!!not base64!!") {
		t.Error("a chunk for another recording, or one that is not base64, was accepted")
	}
	big := base64.StdEncoding.EncodeToString(make([]byte, maxAudioBytes))
	if in.add("7", big) {
		t.Error("a recording longer than the limit was accepted")
	}
	in.begin("9", 16000)
	if len(in.pcm) != 0 || in.id != "9" {
		t.Error("begin did not start afresh")
	}
}

// A stand-in for whisper-cli: prints a fixed line, the way the real one does with -nt.
func TestTranscribeRunsTheCommand(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-whisper")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"-f\" ]; then f=\"$2\"; fi; shift; done\n[ -s \"$f\" ] || exit 3\necho ' [BLANK_AUDIO] show me the diff'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &Transcriber{Bin: bin, Model: "unused", Lang: "en", Threads: 1}
	got, err := tr.Transcribe(context.Background(), make([]byte, 6400), 16000)
	if err != nil || got != "show me the diff" {
		t.Fatalf("got %q, %v", got, err)
	}
	tr.Bin = filepath.Join(dir, "missing")
	if _, err := tr.Transcribe(context.Background(), make([]byte, 6400), 16000); err == nil || !strings.Contains(err.Error(), "whisper failed") {
		t.Errorf("a missing program should fail: %v", err)
	}
}
