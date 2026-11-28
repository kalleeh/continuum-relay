package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"nhooyr.io/websocket"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Speech to text on the relay: a device that has no room for a speech model (the desk companion) sends what it
// recorded and gets text back, so it can type it into a session. The audio goes only to this relay, which runs
// whisper.cpp on it here; nothing is sent anywhere else.
//
//	device -> relay  {"type":"audio_begin","id":"7","rate":16000}
//	device -> relay  {"type":"audio_chunk","id":"7","data":"<base64 of little-endian 16-bit mono PCM>"}  (many)
//	device -> relay  {"type":"audio_end","id":"7"}
//	relay  -> device {"type":"transcript","id":"7","text":"fix the failing test"}
//	               or {"type":"error","code":"transcribe_failed","message":"..."}
//
// It is available when CONTINUUM_WHISPER_BIN (whisper.cpp's whisper-cli) and CONTINUUM_WHISPER_MODEL (a ggml model
// such as ggml-base.en.bin) are set; hello then lists the "transcribe" feature.
const (
	maxAudioSeconds = 40
	maxAudioBytes   = maxAudioSeconds * 16000 * 2
)

// Only a couple of recordings are turned into text at once: whisper uses every core it is given, and a client that sent
// recordings faster than they can be handled would otherwise start a process for each.
var transcribeSlots = make(chan struct{}, 2)

type Transcriber struct {
	Bin, Model, Lang string
	Threads          int
}

var (
	transcriberOnce sync.Once
	transcriber     *Transcriber
)

// CurrentTranscriber is the one configured in the environment, or nil if there is none.
func CurrentTranscriber() *Transcriber {
	transcriberOnce.Do(func() {
		bin, model := os.Getenv("CONTINUUM_WHISPER_BIN"), os.Getenv("CONTINUUM_WHISPER_MODEL")
		if bin == "" || model == "" {
			return
		}
		if _, err := os.Stat(bin); err != nil {
			return
		}
		if _, err := os.Stat(model); err != nil {
			return
		}
		t := &Transcriber{Bin: bin, Model: model, Lang: os.Getenv("CONTINUUM_WHISPER_LANG"), Threads: 4}
		if t.Lang == "" {
			t.Lang = "en"
		}
		if n, err := strconv.Atoi(os.Getenv("CONTINUUM_WHISPER_THREADS")); err == nil && n > 0 {
			t.Threads = n
		}
		transcriber = t
	})
	return transcriber
}

// wavFile wraps 16-bit mono PCM in a WAV header, which is what whisper-cli reads.
func wavFile(pcm []byte, rate int) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

var noiseTagRe = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)|♪+`)

// cleanTranscript drops what whisper prints for silence and noise ("[BLANK_AUDIO]", "(music)") and joins the lines.
func cleanTranscript(s string) string {
	s = noiseTagRe.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

func (t *Transcriber) Transcribe(ctx context.Context, pcm []byte, rate int) (string, error) {
	f, err := os.CreateTemp("", "continuum-audio-*.wav")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(wavFile(pcm, rate)); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Bin, "-m", t.Model, "-f", f.Name(), "-l", t.Lang, "-t", strconv.Itoa(t.Threads), "-nt", "-np")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper failed: %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	return cleanTranscript(out.String()), nil
}

// audioInbox collects the chunks of one recording a client is sending.
type audioInbox struct {
	id   string
	rate int
	pcm  []byte
}

func (a *audioInbox) begin(id string, rate int) {
	if rate < 8000 || rate > 48000 {
		rate = 16000
	}
	a.id, a.rate, a.pcm = id, rate, a.pcm[:0]
}

// add appends a base64 chunk; false if it is not for this recording, not valid, or would make it too long.
func (a *audioInbox) add(id, data string) bool {
	if id != a.id || a.id == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(a.pcm)+len(raw) > maxAudioBytes*a.rate/16000 {
		return false
	}
	a.pcm = append(a.pcm, raw...)
	return true
}

// handleAudio takes one of the three audio messages from a client.
func handleAudio(ctx context.Context, conn *websocket.Conn, inbox *audioInbox, msg ClientMessage) {
	t := CurrentTranscriber()
	switch msg.Type {
	case "audio_begin":
		if t == nil {
			writeError(ctx, conn, "transcribe_unavailable", "this relay has no speech recogniser set up")
			return
		}
		inbox.begin(msg.ID, msg.Rate)
	case "audio_chunk":
		if t != nil && !inbox.add(msg.ID, msg.Data) {
			inbox.id = ""
			writeError(ctx, conn, "audio_invalid", "the audio was not valid or too long")
		}
	case "audio_end":
		if t == nil || msg.ID != inbox.id || inbox.id == "" {
			return
		}
		id, rate := inbox.id, inbox.rate
		pcm := append([]byte(nil), inbox.pcm...)
		inbox.id = ""
		go func() {
			if len(pcm) < rate*2*3/10 { // under 0.3 s: a slip of the finger, not speech
				writeJSON(ctx, conn, map[string]string{"type": "transcript", "id": id, "text": ""})
				return
			}
			select {
			case transcribeSlots <- struct{}{}:
				defer func() { <-transcribeSlots }()
			default:
				writeError(ctx, conn, "transcribe_busy", "the relay is already transcribing")
				return
			}
			text, err := t.Transcribe(ctx, pcm, rate)
			if err != nil {
				writeError(ctx, conn, "transcribe_failed", err.Error())
				return
			}
			writeJSON(ctx, conn, map[string]string{"type": "transcript", "id": id, "text": text})
		}()
	}
}
