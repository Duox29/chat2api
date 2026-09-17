// Package ds is a Go client for DeepSeek's web-chat API
// (chat.deepseek.com), obtained via reverse-engineering the frontend.
//
// Auth relies on a headless Chromium (the installed Playwright build) for
// the initial login + AWS WAF cookies; afterwards all traffic is plain
// HTTPS with a bearer token, device id, cookies and a per-request
// DeepSeekHashV1 proof-of-work (see chat2api/pow).
package ds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"chat2api/pow"
)

const (
	Base          = "https://chat.deepseek.com"
	UA            = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	ClientVersion = "2.5.0"
)

// Session is the persisted auth state (written by scripts/login.js).
type Session struct {
	Token    string `json:"token"`
	DeviceID string `json:"device_id"`
	Did      string `json:"did"`
	Cookies  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"cookies"`
	At int64 `json:"at"`
}

// Client talks to the DeepSeek web API.
type Client struct {
	rootDir    string
	http       *http.Client
	mu         sync.Mutex
	sess       Session
	loginMu    sync.Mutex
	loginSeq   uint64 // successful runLogin count; de-dupes concurrent refresh (guarded by mu)
	lastMsg    map[string]int64 // sessionID -> last assistant message id (for multi-turn)
	tzOffset   string
	apiKey     string
}

// New creates a client rooted at repoDir (expects account.json, scripts/, state/).
func New(repoDir string) *Client {
	_, off := time.Now().Zone()
	c := &Client{
		rootDir: repoDir,
		http:    &http.Client{Timeout: 0},
		lastMsg: map[string]int64{},
		tzOffset: fmt.Sprintf("%d", -off),
	}
	c.loadSession()
	return c
}

func (c *Client) sessionFile() string { return filepath.Join(c.rootDir, "state", "session.json") }

func (c *Client) loadSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := os.Open(c.sessionFile())
	if err != nil {
		return
	}
	defer f.Close()
	var s Session
	if json.NewDecoder(f).Decode(&s) == nil && s.Token != "" {
		c.sess = s
	}
}

func (c *Client) cookieHeader() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, k := range c.sess.Cookies {
		if k.Value == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(k.Name + "=" + k.Value)
	}
	return b.String()
}

func (c *Client) headers(extra map[string]string) http.Header {
	c.mu.Lock()
	tok, dev := c.sess.Token, c.sess.DeviceID
	c.mu.Unlock()
	h := http.Header{}
	h.Set("User-Agent", UA)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", Base)
	h.Set("Referer", Base+"/")
	h.Set("x-client-version", ClientVersion)
	h.Set("x-client-platform", "web")
	h.Set("x-client-locale", "en_US")
	h.Set("x-client-bundle-id", "com.deepseek.chat")
	h.Set("x-client-timezone-offset", c.tzOffset)
	h.Set("x-device-model", "")
	if dev != "" {
		h.Set("x-device-id", dev)
	}
	if tok != "" {
		h.Set("authorization", "Bearer "+tok)
	}
	if ck := c.cookieHeader(); ck != "" {
		h.Set("Cookie", ck)
	}
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// runLogin shells out to the headless-browser login script.
// Concurrent callers serialize on loginMu and de-duplicate: the login
// sequence number is captured before waiting for the lock and re-checked
// inside it, so N concurrent refresh triggers cause a single browser launch
// (~10-30s each). A generation counter (not the token value) is used because
// a refresh may legitimately yield the same token with rotated cookies.
func (c *Client) runLogin(ctx context.Context) error {
	c.mu.Lock()
	before := c.loginSeq
	c.mu.Unlock()
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	c.mu.Lock()
	refreshed := c.loginSeq != before
	c.mu.Unlock()
	if refreshed {
		return nil // another goroutine refreshed while we waited
	}
	cmd := exec.CommandContext(ctx, "node", "scripts/login.js")
	cmd.Dir = c.rootDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("browser login failed: %v: %s", err, truncate(string(out), 500))
	}
	c.loadSession()
	if c.currentToken() == "" {
		return fmt.Errorf("browser login produced no token: %s", truncate(string(out), 300))
	}
	c.mu.Lock()
	c.loginSeq++
	c.mu.Unlock()
	return nil
}

func (c *Client) currentToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess.Token
}

// EnsureLogin loads persisted auth or performs a browser login.
func (c *Client) EnsureLogin(ctx context.Context) error {
	c.loadSession()
	if c.currentToken() == "" {
		return c.runLogin(ctx)
	}
	// Validate token cheaply.
	st, body := c.getJSON(ctx, "/api/v0/chat_session/fetch_page?lte_cursor.pinned=false", nil)
	if st == 200 && bodyCode(body) == 0 {
		return nil
	}
	return c.runLogin(ctx)
}

func bodyCode(body map[string]interface{}) int {
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		return -1
	}
	code, _ := data["biz_code"].(float64)
	return int(code)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}, extra map[string]string) (*http.Response, error) {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, method, Base+path, rdr)
		if err != nil {
			return nil, err
		}
		req.Header = c.headers(extra)
		resp, err := c.http.Do(req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 800 * time.Millisecond):
		}
	}
	return nil, lastErr
}

func (c *Client) getJSON(ctx context.Context, path string, extra map[string]string) (int, map[string]interface{}) {
	resp, err := c.do(ctx, "GET", path, nil, extra)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func (c *Client) postJSON(ctx context.Context, path string, body interface{}, extra map[string]string) (int, map[string]interface{}) {
	resp, err := c.do(ctx, "POST", path, body, extra)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

// needsRelogin reports auth/WAF failures worth a browser re-login.
func needsRelogin(status int) bool { return status == 401 || status == 403 || status == 202 }

// ---------- chat sessions ----------

// ChatSession is a subset of the session object.
type ChatSession struct {
	ID        string  `json:"id"`
	SeqID     int64   `json:"seq_id"`
	ModelType string  `json:"model_type"`
	Title     *string `json:"title"`
	Inserted  float64 `json:"inserted_at"`
	Updated   float64 `json:"updated_at"`
}

func bizData(body map[string]interface{}) map[string]interface{} {
	data, _ := body["data"].(map[string]interface{})
	if data == nil {
		return nil
	}
	bd, _ := data["biz_data"].(map[string]interface{})
	return bd
}

// CreateSession creates a new chat session ("new session").
func (c *Client) CreateSession(ctx context.Context) (*ChatSession, error) {
	for attempt := 0; attempt < 2; attempt++ {
		st, body := c.postJSON(ctx, "/api/v0/chat_session/create", map[string]interface{}{}, nil)
		if needsRelogin(st) && attempt == 0 {
			if err := c.runLogin(ctx); err != nil {
				return nil, err
			}
			continue
		}
		bd := bizData(body)
		if bd == nil {
			return nil, fmt.Errorf("createSession failed (http %d): %s", st, truncate(fmt.Sprint(body), 300))
		}
		raw, _ := json.Marshal(bd["chat_session"])
		var s ChatSession
		if err := json.Unmarshal(raw, &s); err != nil || s.ID == "" {
			return nil, fmt.Errorf("createSession bad payload: %s", truncate(string(raw), 300))
		}
		return &s, nil
	}
	return nil, fmt.Errorf("createSession failed after re-login")
}

// SessionInfo is a listed session.
type SessionInfo struct {
	ID        string  `json:"id"`
	Title     *string `json:"title"`
	ModelType string  `json:"model_type"`
	Updated   float64 `json:"updated_at"`
}

// ListSessions lists recent chat sessions.
func (c *Client) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	st, body := c.getJSON(ctx, "/api/v0/chat_session/fetch_page?lte_cursor.pinned=false", nil)
	if needsRelogin(st) {
		if err := c.runLogin(ctx); err != nil {
			return nil, err
		}
		st, body = c.getJSON(ctx, "/api/v0/chat_session/fetch_page?lte_cursor.pinned=false", nil)
	}
	bd := bizData(body)
	if bd == nil {
		return nil, fmt.Errorf("listSessions failed (http %d)", st)
	}
	raw, _ := json.Marshal(bd["chat_sessions"])
	var out []SessionInfo
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = []SessionInfo{}
	}
	return out, nil
}

// DeleteSession deletes a chat session.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	st, body := c.postJSON(ctx, "/api/v0/chat_session/delete", map[string]interface{}{"chat_session_id": id}, nil)
	if needsRelogin(st) {
		if err := c.runLogin(ctx); err != nil {
			return err
		}
		st, body = c.postJSON(ctx, "/api/v0/chat_session/delete", map[string]interface{}{"chat_session_id": id}, nil)
	}
	if st != 200 || bodyCode(body) != 0 {
		return fmt.Errorf("deleteSession failed (http %d): %s", st, truncate(fmt.Sprint(body), 300))
	}
	return nil
}

// ---------- proof of work ----------

const powTarget = "/api/v0/chat/completion"

func (c *Client) solvePow(ctx context.Context) (string, error) {
	st, body := c.postJSON(ctx, "/api/v0/chat/create_pow_challenge",
		map[string]interface{}{"target_path": powTarget}, nil)
	if needsRelogin(st) {
		if err := c.runLogin(ctx); err != nil {
			return "", err
		}
		st, body = c.postJSON(ctx, "/api/v0/chat/create_pow_challenge",
			map[string]interface{}{"target_path": powTarget}, nil)
	}
	bd := bizData(body)
	if bd == nil {
		return "", fmt.Errorf("create_pow_challenge failed (http %d)", st)
	}
	raw, _ := json.Marshal(bd["challenge"])
	var cw struct {
		Algorithm  string  `json:"algorithm"`
		Challenge  string  `json:"challenge"`
		Salt       string  `json:"salt"`
		Difficulty int     `json:"difficulty"`
		Signature  string  `json:"signature"`
		ExpireAt   int64   `json:"expire_at"`
	}
	if err := json.Unmarshal(raw, &cw); err != nil {
		return "", fmt.Errorf("bad pow challenge: %s", truncate(string(raw), 300))
	}
	ans, err := pow.Solve(pow.Challenge{
		Algorithm: cw.Algorithm, Challenge: cw.Challenge, Salt: cw.Salt,
		Difficulty: cw.Difficulty, Signature: cw.Signature, ExpireAt: cw.ExpireAt,
	})
	if err != nil {
		return "", err
	}
	return pow.HeaderValue(pow.Challenge{
		Algorithm: cw.Algorithm, Challenge: cw.Challenge, Salt: cw.Salt,
		Signature: cw.Signature,
	}, ans, powTarget), nil
}
