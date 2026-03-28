package apns

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

const apnsURL = "https://api.push.apple.com/3/device/"

// Client sends APNs push notifications using the HTTP/2 provider API with JWT auth.
// All fields are required. Build with New(); nil Client means push is disabled.
type Client struct {
	keyID      string
	teamID     string
	bundleID   string
	key        *ecdsa.PrivateKey
	httpClient *http.Client

	mu       sync.Mutex
	jwt      string
	jwtExp   time.Time
}

// New parses the p8 key file and returns a ready-to-use Client.
// keyPath is the path to the .p8 file from Apple Developer Portal.
func New(keyPath, keyID, teamID, bundleID string) (*Client, error) {
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading APNs key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("invalid PEM block in APNs key file")
	}
	iface, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing APNs key: %w", err)
	}
	key, ok := iface.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("APNs key is not an ECDSA key")
	}
	return &Client{
		keyID:      keyID,
		teamID:     teamID,
		bundleID:   bundleID,
		key:        key,
		httpClient: &http.Client{},
	}, nil
}

// Send delivers a SESSION_FINISHED alert to the given APNs device token.
// sessionID is included in the payload so the app can open the correct session.
func (c *Client) Send(deviceToken, title, body, sessionID string) error {
	jwt, err := c.getJWT()
	if err != nil {
		return fmt.Errorf("building APNs JWT: %w", err)
	}

	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": title,
				"body":  body,
			},
			"sound":    "default",
			"category": "SESSION_FINISHED",
		},
		"sessionId": sessionID,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, apnsURL+deviceToken, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("apns-topic", c.bundleID)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("APNs request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		return fmt.Errorf("APNs returned %d: %s", resp.StatusCode, errResp.Reason)
	}
	return nil
}

// getJWT returns a cached JWT, rebuilding it if it has expired.
func (c *Client) getJWT() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jwt != "" && time.Now().Before(c.jwtExp) {
		return c.jwt, nil
	}
	t, err := c.buildJWT()
	if err != nil {
		return "", err
	}
	c.jwt = t
	c.jwtExp = time.Now().Add(55 * time.Minute) // APNs JWTs are valid for 60 min
	return t, nil
}

// buildJWT constructs an ES256-signed JWT for APNs provider auth.
func (c *Client) buildJWT() (string, error) {
	headerJSON, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.keyID})
	payloadJSON, _ := json.Marshal(map[string]any{"iss": c.teamID, "iat": time.Now().Unix()})

	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := header + "." + payload

	hash := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, hash[:])
	if err != nil {
		return "", err
	}

	// ES256 signature is fixed-length 64 bytes: 32-byte r || 32-byte s (P-256 curve)
	sig := make([]byte, 64)
	rBytes := r.Bytes()
	sBytes := s.Bytes()
	copy(sig[32-len(rBytes):32], rBytes)
	copy(sig[64-len(sBytes):64], sBytes)

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
