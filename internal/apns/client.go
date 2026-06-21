package apns

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
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

	mu     sync.Mutex
	jwt    string
	jwtExp time.Time
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
		keyID:    keyID,
		teamID:   teamID,
		bundleID: bundleID,
		key:      key,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
				ForceAttemptHTTP2:     true,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
	}, nil
}

// ErrTokenInvalid signals the device token is permanently dead (410 Unregistered
// or 400 BadDeviceToken) and callers should delete it. Distinct from transient
// failures (429/500/503/403-JWT), which are worth retrying with the same token.
var ErrTokenInvalid = errors.New("apns: device token invalid")

// Alert describes a user-visible alert push.
type Alert struct {
	Title     string
	Body      string
	SessionID string // payload "sessionId" — lets the app open the right session on tap
	Category  string // UNNotificationCategory id; defaults to "SESSION_FINISHED"
	// CollapseID coalesces notifications: a new push with the same id replaces an
	// earlier one still displayed/pending on the device (≤64 bytes). Empty = no
	// collapsing. Used to dedup repeats of the same logical event per session.
	CollapseID string
}

// Send delivers a user-visible alert to the given APNs device token. Returns
// ErrTokenInvalid (wrapped) when APNs reports the token is dead, so the caller
// can drop it; other non-2xx responses return a transient error.
func (c *Client) Send(deviceToken string, alert Alert) error {
	jwt, err := c.getJWT()
	if err != nil {
		return fmt.Errorf("building APNs JWT: %w", err)
	}

	category := alert.Category
	if category == "" {
		category = "SESSION_FINISHED"
	}
	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]string{
				"title": alert.Title,
				"body":  alert.Body,
			},
			"sound":    "default",
			"category": category,
		},
		"sessionId": alert.SessionID,
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
	req.Header.Set("apns-priority", "10") // user-visible alert → deliver immediately
	// Finite expiration so a queued alert isn't delivered hours late, after it
	// stopped being relevant. ~1h is plenty for an "attention needed" prompt.
	req.Header.Set("apns-expiration", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	if alert.CollapseID != "" {
		id := alert.CollapseID
		if len(id) > 64 { // APNs hard limit
			id = id[:64]
		}
		req.Header.Set("apns-collapse-id", id)
	}
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
		// 403 ExpiredProviderToken means the cached JWT is no longer accepted
		// (typically clock skew eating the 55-min margin). Drop the cache so the
		// next Send rebuilds a fresh token instead of failing for up to 55 min.
		if resp.StatusCode == http.StatusForbidden {
			c.invalidateJWT()
		}
		// 410 Unregistered / 400 BadDeviceToken mean the token is permanently
		// dead — signal the caller to delete it.
		if resp.StatusCode == http.StatusGone || errResp.Reason == "BadDeviceToken" {
			return fmt.Errorf("%w (%d %s)", ErrTokenInvalid, resp.StatusCode, errResp.Reason)
		}
		return fmt.Errorf("APNs returned %d: %s", resp.StatusCode, errResp.Reason)
	}
	return nil
}

// SendLiveActivity updates an iOS Live Activity via APNs. activityToken is the
// per-activity push token the app obtained from Activity.pushTokenUpdates (NOT
// the device token). contentState is the JSON the widget's ContentState decodes
// (status / isRunning / lastActivityAt). The apns-topic for Live Activities is
// the app bundle id with the ".push-type.liveactivity" suffix.
//
// Returns the HTTP status reason on failure; a 410 means the token is stale
// (activity ended) and the caller should drop it.
func (c *Client) SendLiveActivity(activityToken string, contentState map[string]any, staleAfter time.Duration) error {
	jwt, err := c.getJWT()
	if err != nil {
		return fmt.Errorf("building APNs JWT: %w", err)
	}

	aps := map[string]any{
		"timestamp":     time.Now().Unix(),
		"event":         "update",
		"content-state": contentState,
	}
	if staleAfter > 0 {
		aps["stale-date"] = time.Now().Add(staleAfter).Unix()
	}
	data, err := json.Marshal(map[string]any{"aps": aps})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, apnsURL+activityToken, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("apns-topic", c.bundleID+".push-type.liveactivity")
	req.Header.Set("apns-push-type", "liveactivity")
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
		if resp.StatusCode == http.StatusForbidden {
			c.invalidateJWT()
		}
		return fmt.Errorf("APNs returned %d: %s", resp.StatusCode, errResp.Reason)
	}
	return nil
}

// invalidateJWT clears the cached token so the next getJWT rebuilds it.
func (c *Client) invalidateJWT() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jwt = ""
	c.jwtExp = time.Time{}
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
