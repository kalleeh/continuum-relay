package relay

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/continuum-app/continuum-relay/internal/auth"
)

func resetTranscriberForTest() {
	transcriberOnce = sync.Once{}
	transcriber = nil
}

// A real websocket connection to HandleClient, the way the desk device and the phone talk to it.
func dialClient(t *testing.T) (*websocket.Conn, context.Context) {
	t.Helper()
	hub := NewHub(nil)
	a := auth.New("test-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		HandleClient(r.Context(), conn, hub, a, "test-client")
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn, ctx
}

func send(t *testing.T, ctx context.Context, conn *websocket.Conn, v any) {
	t.Helper()
	data, _ := json.Marshal(v)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

// reads messages until one of the given type arrives (status broadcasts and the like may come first)
func expect(t *testing.T, ctx context.Context, conn *websocket.Conn, typ string) map[string]any {
	t.Helper()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v", typ, err)
		}
		var m map[string]any
		if json.Unmarshal(data, &m) == nil && m["type"] == typ {
			return m
		}
	}
}

func TestHelloOverAConnection(t *testing.T) {
	resetTranscriberForTest()
	t.Cleanup(resetTranscriberForTest)
	conn, ctx := dialClient(t)
	send(t, ctx, conn, map[string]any{"type": "hello"})
	m := expect(t, ctx, conn, "hello")
	if m["protocol"].(float64) != ProtocolVersion {
		t.Errorf("protocol %v", m["protocol"])
	}
	features := strings.Join(toStrings(m["features"]), ",")
	for _, f := range []string{"start_session", "send_key", "capture_screen", "styled_screen"} {
		if !strings.Contains(features, f) {
			t.Errorf("feature %q missing from %q", f, features)
		}
	}
	if strings.Contains(features, "transcribe") {
		t.Errorf("transcribe is listed without a speech recogniser: %q", features)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Recording, in pieces, turned into text by (a stand-in for) whisper, and the ways it can go wrong.
func TestAudioOverAConnection(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-whisper")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"-f\" ]; then f=\"$2\"; fi; shift; done\n[ -s \"$f\" ] || exit 3\necho ' open the settings file '\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(dir, "model.bin")
	os.WriteFile(model, []byte("x"), 0o644)
	t.Setenv("CONTINUUM_WHISPER_BIN", bin)
	t.Setenv("CONTINUUM_WHISPER_MODEL", model)
	resetTranscriberForTest()
	t.Cleanup(resetTranscriberForTest)

	conn, ctx := dialClient(t)
	send(t, ctx, conn, map[string]any{"type": "hello"})
	if !strings.Contains(strings.Join(toStrings(expect(t, ctx, conn, "hello")["features"]), ","), "transcribe") {
		t.Fatal("hello does not list transcribe although a recogniser is set up")
	}

	pcm := make([]byte, 16000*2) // a second of silence
	piece := func(from, to int) string { return base64.StdEncoding.EncodeToString(pcm[from:to]) }
	send(t, ctx, conn, map[string]any{"type": "audio_begin", "id": "7", "rate": 16000})
	send(t, ctx, conn, map[string]any{"type": "audio_chunk", "id": "7", "data": piece(0, 16000)})
	send(t, ctx, conn, map[string]any{"type": "audio_chunk", "id": "7", "data": piece(16000, 32000)})
	send(t, ctx, conn, map[string]any{"type": "audio_end", "id": "7"})
	m := expect(t, ctx, conn, "transcript")
	if m["id"] != "7" || m["text"] != "open the settings file" {
		t.Errorf("transcript %v", m)
	}

	// a slip of the finger (under 0.3 s) answers with nothing, without running whisper
	send(t, ctx, conn, map[string]any{"type": "audio_begin", "id": "8", "rate": 16000})
	send(t, ctx, conn, map[string]any{"type": "audio_chunk", "id": "8", "data": piece(0, 1000)})
	send(t, ctx, conn, map[string]any{"type": "audio_end", "id": "8"})
	if m := expect(t, ctx, conn, "transcript"); m["id"] != "8" || m["text"] != "" {
		t.Errorf("short recording: %v", m)
	}

	// a chunk that is not valid base64 is refused, and the recording is dropped
	send(t, ctx, conn, map[string]any{"type": "audio_begin", "id": "9", "rate": 16000})
	send(t, ctx, conn, map[string]any{"type": "audio_chunk", "id": "9", "data": "!!!"})
	if m := expect(t, ctx, conn, "error"); m["code"] != "audio_invalid" {
		t.Errorf("bad chunk: %v", m)
	}
	send(t, ctx, conn, map[string]any{"type": "audio_end", "id": "9"}) // nothing to finish: silently ignored
}

func TestAudioWithoutARecogniser(t *testing.T) {
	resetTranscriberForTest()
	t.Cleanup(resetTranscriberForTest)
	conn, ctx := dialClient(t)
	send(t, ctx, conn, map[string]any{"type": "audio_begin", "id": "1", "rate": 16000})
	if m := expect(t, ctx, conn, "error"); m["code"] != "transcribe_unavailable" {
		t.Errorf("%v", m)
	}
}
