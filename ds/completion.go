package ds

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Stream events from a chat completion.
type Event struct {
	Type      string // "text", "thinking", "ready", "title", "done"
	Delta     string
	Title     string
	ReqID     int64
	RespID    int64
	ModelType string
}

type fragment struct {
	typ     string
	content string
}

// CompletionOptions controls one completion request.
type CompletionOptions struct {
	SessionID       string
	Prompt          string
	ModelType       string // "default" | "expert" | "vision"
	ThinkingEnabled bool
	SearchEnabled   bool
	ParentMessageID *int64 // nil = auto (last message of session / root)
}

// StreamCompletion POSTs /api/v0/chat/completion and calls emit for each
// event. It returns the final assistant message id (for multi-turn).
func (c *Client) StreamCompletion(ctx context.Context, o CompletionOptions, emit func(Event)) (int64, error) {
	parent, err := c.resolveParent(ctx, o)
	if err != nil {
		return 0, err
	}
	powHeader, err := c.solvePow(ctx)
	if err != nil {
		return 0, err
	}
	body := map[string]interface{}{
		"chat_session_id":  o.SessionID,
		"parent_message_id": parent,
		"model_type":        o.ModelType,
		"prompt":            o.Prompt,
		"ref_file_ids":      []string{},
		"thinking_enabled":  o.ThinkingEnabled,
		"search_enabled":    o.SearchEnabled,
		"action":            nil,
		"preempt":           false,
	}
	resp, err := c.do(ctx, "POST", "/api/v0/chat/completion", body,
		map[string]string{
			"x-ds-pow-response": powHeader,
			"Referer":           Base + "/a/chat/s/" + o.SessionID,
		})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if needsRelogin(resp.StatusCode) {
		if err := c.runLogin(ctx); err != nil {
			return 0, err
		}
		return c.StreamCompletion(ctx, o, emit)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("completion HTTP %d: %s", resp.StatusCode, truncate(string(b), 500))
	}

	var frags []fragment
	fragType := func(i int) string {
		if i >= 0 && i < len(frags) && frags[i].typ != "" {
			return frags[i].typ
		}
		return "RESPONSE"
	}
	kindOf := func(t string) string {
		if t == "THINK" || t == "THINKING" {
			return "thinking"
		}
		return "text"
	}

	var respID int64
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 1024*1024), 1024*1024)
	var evName string
	var dataLines []string
	flush := func() {
		defer func() { evName = ""; dataLines = nil }()
		if len(dataLines) == 0 {
			return
		}
		data := strings.TrimSpace(strings.Join(dataLines, "\n"))
		switch evName {
		case "close":
			emit(Event{Type: "done"})
			return
		case "title":
			var t struct {
				Content string `json:"content"`
			}
			if json.Unmarshal([]byte(data), &t) == nil {
				emit(Event{Type: "title", Title: t.Content})
			}
			return
		}
		// ready / data payloads
		var d map[string]interface{}
		if json.Unmarshal([]byte(data), &d) != nil {
			return
		}
		if evName == "ready" || hasKey(d, "request_message_id") {
			if v, ok := num(d, "request_message_id"); ok {
				_ = v
			}
			if v, ok := num(d, "response_message_id"); ok {
				respID = int64(v)
			}
			if mt, ok := d["model_type"].(string); ok {
				emit(Event{Type: "ready", ReqID: 0, RespID: respID, ModelType: mt})
			} else {
				emit(Event{Type: "ready", RespID: respID})
			}
			return
		}
		v, _ := d["v"]
		// Full snapshot: {"v": {"response": {"fragments": [...]}}}
		if vm, ok := v.(map[string]interface{}); ok {
			if r, ok := vm["response"].(map[string]interface{}); ok {
				if fl, ok := r["fragments"].([]interface{}); ok {
					for i, fi := range fl {
						fm, _ := fi.(map[string]interface{})
						ct, _ := fm["content"].(string)
						tp, _ := fm["type"].(string)
						if i < len(frags) {
							// emit only the unseen suffix (defensive; normally
							// the snapshot arrives once, before any APPEND)
							if len(ct) > len(frags[i].content) {
								emit(Event{Type: kindOf(tp), Delta: ct[len(frags[i].content):]})
							}
							frags[i] = fragment{typ: tp, content: ct}
						} else {
							frags = append(frags, fragment{typ: tp, content: ct})
							if ct != "" {
								emit(Event{Type: kindOf(tp), Delta: ct})
							}
						}
					}
					return
				}
			}
		}
		p, _ := d["p"].(string)
		op, _ := d["o"].(string)
		if op == "APPEND" && strings.HasPrefix(p, "response/fragments/") && strings.HasSuffix(p, "/content") {
			mid := strings.TrimSuffix(strings.TrimPrefix(p, "response/fragments/"), "/content")
			var idx int
			if mid == "-1" {
				idx = len(frags) - 1
			} else {
				fmt.Sscanf(mid, "%d", &idx)
			}
			if s, ok := v.(string); ok {
				t := fragType(idx)
				emit(Event{Type: kindOf(t), Delta: s})
				if idx >= 0 && idx < len(frags) {
					frags[idx].content += s
				}
			}
			return
		}
		// Bare continuation {"v": "..."} belongs to the last fragment.
		if s, ok := v.(string); ok && s != "" && p == "" {
			t := fragType(len(frags) - 1)
			emit(Event{Type: kindOf(t), Delta: s})
			if len(frags) > 0 {
				frags[len(frags)-1].content += s
			}
		}
	}

	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment / heartbeat
		}
		if strings.HasPrefix(line, "event:") {
			evName = strings.TrimSpace(line[len("event:"):])
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(line[len("data:"):]))
		}
	}
	flush()
	if err := scan.Err(); err != nil {
		return respID, err
	}
	if respID != 0 {
		c.mu.Lock()
		c.lastMsg[o.SessionID] = respID
		c.mu.Unlock()
	}
	return respID, nil
}

func hasKey(m map[string]interface{}, k string) bool {
	_, ok := m[k]
	return ok
}

func num(m map[string]interface{}, k string) (float64, bool) {
	v, ok := m[k].(float64)
	return v, ok
}

// resolveParent determines parent_message_id for continuity.
func (c *Client) resolveParent(ctx context.Context, o CompletionOptions) (*int64, error) {
	if o.ParentMessageID != nil {
		return o.ParentMessageID, nil
	}
	c.mu.Lock()
	id, ok := c.lastMsg[o.SessionID]
	c.mu.Unlock()
	if ok {
		return &id, nil
	}
	// Fallback: latest assistant message from history.
	if mid, ok := c.latestAssistantMsg(ctx, o.SessionID); ok {
		c.mu.Lock()
		c.lastMsg[o.SessionID] = mid
		c.mu.Unlock()
		return &mid, nil
	}
	return nil, nil // root message
}

// latestAssistantMsg returns the newest message id in a session, if any.
func (c *Client) latestAssistantMsg(ctx context.Context, sessionID string) (int64, bool) {
	st, body := c.getJSON(ctx, "/api/v0/chat/history_messages?chat_session_id="+sessionID, nil)
	if needsRelogin(st) {
		if err := c.runLogin(ctx); err != nil {
			return 0, false
		}
		st, body = c.getJSON(ctx, "/api/v0/chat/history_messages?chat_session_id="+sessionID, nil)
	}
	bd := bizData(body)
	if bd == nil {
		return 0, false
	}
	msgs, _ := bd["chat_messages"].([]interface{})
	var best int64
	for _, m := range msgs {
		mm, _ := m.(map[string]interface{})
		if id, ok := num(mm, "message_id"); ok && int64(id) > best {
			best = int64(id)
		}
	}
	return best, best != 0
}

// Complete is the non-streaming convenience wrapper.
func (c *Client) Complete(ctx context.Context, o CompletionOptions) (text, thinking, title string, err error) {
	_, err = c.StreamCompletion(ctx, o, func(e Event) {
		switch e.Type {
		case "text":
			text += e.Delta
		case "thinking":
			thinking += e.Delta
		case "title":
			title = e.Title
		}
	})
	return text, thinking, title, err
}
