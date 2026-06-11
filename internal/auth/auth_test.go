package auth

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func bearerReq(ip, token string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = ip + ":12345"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func basicReq(ip, user, token string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = ip + ":12345"
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	r.Header.Set("Authorization", "Basic "+cred)
	return r
}

func TestValidateRequest_GoodAndBad(t *testing.T) {
	a := New("secret")
	if !a.ValidateRequest(bearerReq("10.0.0.1", "secret")) {
		t.Fatal("correct token rejected")
	}
	if a.ValidateRequest(bearerReq("10.0.0.1", "wrong")) {
		t.Fatal("wrong token accepted")
	}
	if a.ValidateRequest(bearerReq("10.0.0.1", "")) {
		t.Fatal("missing token accepted")
	}
}

// TestLockout_AfterMaxFailures is the regression guard for the rate-limiting
// gap: after maxFailures bad attempts from one IP, even a CORRECT token from
// that IP is refused until the window expires.
func TestLockout_AfterMaxFailures(t *testing.T) {
	a := New("secret")
	for i := 0; i < maxFailures; i++ {
		a.ValidateRequest(bearerReq("10.0.0.2", "wrong"))
	}
	if a.ValidateRequest(bearerReq("10.0.0.2", "secret")) {
		t.Fatal("correct token accepted while IP is locked out")
	}
	// A different IP is unaffected.
	if !a.ValidateRequest(bearerReq("10.0.0.3", "secret")) {
		t.Fatal("unrelated IP penalized by another IP's failures")
	}
}

func TestSuccessClearsFailures(t *testing.T) {
	a := New("secret")
	for i := 0; i < maxFailures-1; i++ {
		a.ValidateRequest(bearerReq("10.0.0.4", "wrong"))
	}
	// One success resets the counter…
	if !a.ValidateRequest(bearerReq("10.0.0.4", "secret")) {
		t.Fatal("correct token rejected before lockout threshold")
	}
	// …so we can fail maxFailures-1 more times without being locked out.
	for i := 0; i < maxFailures-1; i++ {
		a.ValidateRequest(bearerReq("10.0.0.4", "wrong"))
	}
	if !a.ValidateRequest(bearerReq("10.0.0.4", "secret")) {
		t.Fatal("failures were not cleared by the intervening success")
	}
}

// TestValidateBasic mirrors what the terminal endpoint relies on: the Basic
// username must match and the password half is the token. It shares the same
// authenticator (and therefore lockout) as the Bearer path.
func TestValidateBasic(t *testing.T) {
	a := New("secret")
	if !a.ValidateBasic(basicReq("10.0.0.5", "continuum", "secret"), "continuum") {
		t.Fatal("correct Basic credential rejected")
	}
	if a.ValidateBasic(basicReq("10.0.0.5", "continuum", "wrong"), "continuum") {
		t.Fatal("wrong Basic token accepted")
	}
	if a.ValidateBasic(basicReq("10.0.0.5", "attacker", "secret"), "continuum") {
		t.Fatal("wrong Basic username accepted")
	}
}

// TestUpdateToken_RotationTakesEffect is the regression guard for the bug where
// rotate_token left the terminal endpoint validating the OLD token: after
// UpdateToken, the old token must fail and the new one succeed on the same
// authenticator both servers share.
func TestUpdateToken_RotationTakesEffect(t *testing.T) {
	a := New("old")
	if !a.ValidateBasic(basicReq("10.0.0.6", "continuum", "old"), "continuum") {
		t.Fatal("old token should work before rotation")
	}
	a.UpdateToken("new")
	if a.ValidateBasic(basicReq("10.0.0.7", "continuum", "old"), "continuum") {
		t.Fatal("old token still accepted after rotation")
	}
	if !a.ValidateBasic(basicReq("10.0.0.7", "continuum", "new"), "continuum") {
		t.Fatal("new token rejected after rotation")
	}
}
