// Command chat2api converts DeepSeek web chat into an OpenAI-compatible API.
//
//	Endpoints:
//	  GET    /health
//	  GET    /v1/models
//	  POST   /v1/sessions            new chat session
//	  GET    /v1/sessions            list chat sessions
//	  DELETE /v1/sessions/{id}       delete a chat session
//	  POST   /v1/chat/completions    chat completion (stream + non-stream)
//	  POST   /v1/completions         legacy text completion
//
//	Env:
//	  PORT=8080  GATEWAY_API_KEY=<optional>  CHROME_PATH=<optional>
//	  SESSION_AFFINITY=1 (default on: same local thread reuses one web session,
//	    /new resets history -> new web session; 0 to disable, always new)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"chat2api/ds"
)

type Model struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Created   int64  `json:"created"`
	OwnedBy   string `json:"owned_by"`
	ModelType string `json:"-"`
	Thinking  bool   `json:"-"`
}

var models = []Model{
	{ID: "deepseek-chat", Object: "model", Created: 1789616000, OwnedBy: "deepseek", ModelType: "default", Thinking: false},
	{ID: "deepseek-reasoner", Object: "model", Created: 1789616000, OwnedBy: "deepseek", ModelType: "default", Thinking: true},
	{ID: "deepseek-expert", Object: "model", Created: 1789616000, OwnedBy: "deepseek", ModelType: "expert", Thinking: false},
	{ID: "deepseek-vision", Object: "model", Created: 1789616000, OwnedBy: "deepseek", ModelType: "vision", Thinking: false},
}

func resolveModel(name string) Model {
	for _, m := range models {
		if m.ID == name {
			return m
		}
	}
	return models[0]
}

// ---- session affinity ----
// Maps a stateless OpenAI-style conversation (full messages[] each request)
// onto a stateful DeepSeek web session:
//   - same local thread (new messages[] extends previous messages[]) reuses
//     the same web chat_session_id and sends only the incremental tail;
//   - /new (history reset / divergent) no longer matches any prefix, so a
//     fresh web session is created automatically.
// Explicit session_id from the client always wins; "new"/"auto" forces a
// fresh web session. Disable with SESSION_AFFINITY=0|false|no|off.
const affinityMaxEntries = 32

type affinityMsg struct {
	Role       string
	Content    string
	Name       string
	ToolCallID string
	ToolCalls  string
}

type affinityEntry struct {
	SessionID string
	Messages  []affinityMsg
	Prompt    string // last full legacy prompt (for /v1/completions)
	Updated   time.Time
}

var (
	affinityMu      sync.Mutex
	affinityEntries []affinityEntry
)

func sessionAffinityEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SESSION_AFFINITY"))) {
	case "", "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func isAutoSessionID(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "", "new", "_new", "auto":
		return true
	default:
		return false
	}
}

func normalizeAffinityMessages(msgs []map[string]interface{}) []affinityMsg {
	out := make([]affinityMsg, 0, len(msgs))
	for _, m := range msgs {
		role, _ := m["role"].(string)
		name, _ := m["name"].(string)
		callID, _ := m["tool_call_id"].(string)
		if callID == "" {
			callID, _ = m["id"].(string)
		}
		tcSig := ""
		if raw, ok := m["tool_calls"].([]interface{}); ok {
			var parts []string
			for _, tc := range raw {
				tm, _ := tc.(map[string]interface{})
				if tm == nil {
					continue
				}
				id, _ := tm["id"].(string)
				fn, _ := tm["function"].(map[string]interface{})
				fname, args := "", ""
				if fn != nil {
					fname, _ = fn["name"].(string)
					args, _ = fn["arguments"].(string)
					if args == "" {
						if av, ok := fn["arguments"]; ok && av != nil {
							args = fmt.Sprint(av)
						}
					}
				} else {
					fname, _ = tm["name"].(string)
				}
				parts = append(parts, id+"|"+fname+"|"+args)
			}
			tcSig = strings.Join(parts, ";")
		}
		out = append(out, affinityMsg{
			Role: role, Content: messageContentToString(m["content"]),
			Name: name, ToolCallID: callID, ToolCalls: tcSig,
		})
	}
	return out
}

func affinityPrefixMatch(old, cur []affinityMsg) bool {
	if len(old) == 0 || len(old) > len(cur) {
		return false
	}
	for i := range old {
		if old[i] != cur[i] {
			return false
		}
	}
	return true
}

// findAffinity returns the session whose stored messages are a prefix of cur.
// Best match = longest prefix, tie-break most recently updated. Caller must
// not hold affinityMu.
func findAffinity(cur []affinityMsg) *affinityEntry {
	affinityMu.Lock()
	defer affinityMu.Unlock()
	var best *affinityEntry
	for i := range affinityEntries {
		e := &affinityEntries[i]
		if len(e.Messages) == 0 {
			continue
		}
		if !affinityPrefixMatch(e.Messages, cur) {
			continue
		}
		if best == nil || len(e.Messages) > len(best.Messages) ||
			(len(e.Messages) == len(best.Messages) && e.Updated.After(best.Updated)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

func findAffinityByPrompt(prompt string) *affinityEntry {
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	affinityMu.Lock()
	defer affinityMu.Unlock()
	var best *affinityEntry
	for i := range affinityEntries {
		e := &affinityEntries[i]
		if e.Prompt == "" || len(e.Messages) > 0 {
			continue
		}
		if !strings.HasPrefix(prompt, e.Prompt) {
			continue
		}
		if best == nil || len(e.Prompt) > len(best.Prompt) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	cp := *best
	return &cp
}

func upsertAffinity(sessionID string, msgs []map[string]interface{}, prompt string) {
	if sessionID == "" {
		return
	}
	norm := normalizeAffinityMessages(msgs)
	affinityMu.Lock()
	defer affinityMu.Unlock()
	for i := range affinityEntries {
		if affinityEntries[i].SessionID == sessionID {
			affinityEntries[i].Messages = norm
			affinityEntries[i].Prompt = prompt
			affinityEntries[i].Updated = time.Now()
			return
		}
	}
	affinityEntries = append(affinityEntries, affinityEntry{
		SessionID: sessionID, Messages: norm, Prompt: prompt, Updated: time.Now(),
	})
	if len(affinityEntries) > affinityMaxEntries {
		// Evict oldest.
		oldest := 0
		for i := range affinityEntries {
			if affinityEntries[i].Updated.Before(affinityEntries[oldest].Updated) {
				oldest = i
			}
		}
		affinityEntries = append(affinityEntries[:oldest], affinityEntries[oldest+1:]...)
	}
}

func upsertAffinityPrompt(sessionID, prompt string) {
	if sessionID == "" {
		return
	}
	affinityMu.Lock()
	defer affinityMu.Unlock()
	for i := range affinityEntries {
		if affinityEntries[i].SessionID == sessionID {
			affinityEntries[i].Prompt = prompt
			affinityEntries[i].Updated = time.Now()
			return
		}
	}
	affinityEntries = append(affinityEntries, affinityEntry{
		SessionID: sessionID, Prompt: prompt, Updated: time.Now(),
	})
	if len(affinityEntries) > affinityMaxEntries {
		affinityEntries = affinityEntries[len(affinityEntries)-affinityMaxEntries:]
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{"message": msg, "type": "invalid_request_error"},
	})
}

// dsmlDebugEnabled reports whether DSML raw logging is enabled.
// Opt-in via DSML_DEBUG=1|true|yes|raw|on. Logs go to stderr, truncated.
// It NEVER logs secrets: no API keys, tokens, cookies, or auth headers —
// only the DeepSeek prompt/response text and the parsed tool calls.
func dsmlDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSML_DEBUG"))) {
	case "1", "true", "yes", "raw", "on":
		return true
	}
	return false
}

// dsmlParsingEnabled reports whether DSML → tool_calls translation is active.
// Default ON; set DSML_ENABLED=0|false|no|off to pass DeepSeek response text
// through verbatim (pre-parser behavior, no tool_calls ever emitted).
func dsmlParsingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSML_ENABLED"))) {
	case "", "1", "true", "yes", "on":
		return true
	}
	return false
}

// adaptDSML converts raw DeepSeek response text into OpenAI message content,
// tool_calls and finish_reason, honoring the DSML_ENABLED toggle.
func adaptDSML(text string) (content string, toolCalls []interface{}, finish string) {
	if !dsmlParsingEnabled() {
		return text, nil, "stop"
	}
	parsed := ds.ParseDSML(text)
	if len(parsed.Calls) == 0 {
		return parsed.CleanText, nil, "stop"
	}
	return parsed.CleanText, dsmlToOpenAIToolCalls(parsed.Calls), "tool_calls"
}

// previewRunes truncates s to n runes (rune-safe: never splits ｜ etc.).
func previewRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf("…[%d more runes]", len(r)-n)
}

// logDSMLRaw logs one DeepSeek response through the DSML adapter:
// raw length + truncated raw text, detection result, clean text and the
// normalized tool calls. No-op unless DSML_DEBUG is set.
func logDSMLRaw(tag, raw string, res ds.ParseResult) {
	if !dsmlDebugEnabled() {
		return
	}
	log.Printf("[dsml] %s: raw_len=%d hasDSML=%v calls=%d clean=%q",
		tag, len(raw), res.HasDSML, len(res.Calls), previewRunes(res.CleanText, 500))
	log.Printf("[dsml] %s raw preview: %q", tag, previewRunes(raw, 4000))
	for i, c := range res.Calls {
		log.Printf("[dsml] %s call[%d]: name=%q args=%s",
			tag, i, c.Name, previewRunes(c.ArgumentsJSON(), 1000))
	}
}

// messageContentToString flattens OpenAI content (string | content-parts[]).
func messageContentToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []interface{}:
		var b strings.Builder
		for _, p := range t {
			pm, _ := p.(map[string]interface{})
			if pm == nil {
				continue
			}
			switch pm["type"] {
			case "text":
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s + "\n")
				}
			case "input_text":
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s + "\n")
				}
			}
		}
		return strings.TrimRight(b.String(), "\n")
	}
	return fmt.Sprint(v)
}

// formatIncomingToolCalls renders an assistant message's tool_calls for the
// DeepSeek prompt so multi-turn agentic loops keep working.
func formatIncomingToolCalls(m map[string]interface{}) string {
	raw, ok := m["tool_calls"].([]interface{})
	if !ok || len(raw) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nAssistant tool calls:")
	for _, tc := range raw {
		tm, _ := tc.(map[string]interface{})
		if tm == nil {
			continue
		}
		id, _ := tm["id"].(string)
		fn, _ := tm["function"].(map[string]interface{})
		name := ""
		args := ""
		if fn != nil {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
			if args == "" {
				if av, ok := fn["arguments"]; ok && av != nil {
					args = fmt.Sprint(av)
				}
			}
		}
		if name == "" {
			name, _ = tm["name"].(string)
		}
		fmt.Fprintf(&b, "\n- id=%s name=%s arguments=%s", id, name, args)
	}
	return b.String()
}

// messagesToPrompt flattens OpenAI messages[] into one DeepSeek prompt,
// preserving assistant tool_calls and tool results for agentic follow-ups.
func messagesToPrompt(messages []map[string]interface{}) string {
	if len(messages) == 0 {
		return ""
	}
	if len(messages) == 1 {
		m := messages[0]
		s := messageContentToString(m["content"])
		if tc := formatIncomingToolCalls(m); tc != "" {
			s += tc
		}
		return s
	}
	var b strings.Builder
	for _, m := range messages {
		role, _ := m["role"].(string)
		content := messageContentToString(m["content"])
		switch role {
		case "system":
			b.WriteString("System: " + content + "\n\n")
		case "assistant":
			b.WriteString("Assistant: " + content + formatIncomingToolCalls(m) + "\n\n")
		case "tool", "function":
			name, _ := m["name"].(string)
			callID, _ := m["tool_call_id"].(string)
			if callID == "" {
				callID, _ = m["id"].(string)
			}
			if name != "" && callID != "" {
				fmt.Fprintf(&b, "Tool result for %s (id %s):\n%s\n\n", name, callID, content)
			} else if name != "" {
				fmt.Fprintf(&b, "Tool result for %s:\n%s\n\n", name, content)
			} else {
				b.WriteString("Tool result:\n" + content + "\n\n")
			}
		default:
			b.WriteString("User: " + content + "\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}

// appendToolsSection translates the OpenAI tools[] list into DSML usage
// instructions appended to the DeepSeek prompt. This is a protocol translation
// (DeepSeek web has no native function-calling field), not prompt engineering
// to suppress DSML: it tells the model HOW to emit DSML for the declared tools.
func appendToolsSection(prompt string, tools []map[string]interface{}) string {
	if len(tools) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\nAvailable tools (invoke them with DSML blocks). Schemas (JSON):")
	for _, t := range tools {
		name := ""
		desc := ""
		params := ""
		if fn, ok := t["function"].(map[string]interface{}); ok {
			name, _ = fn["name"].(string)
			desc, _ = fn["description"].(string)
			if p, ok := fn["parameters"]; ok && p != nil {
				if pb, err := json.Marshal(p); err == nil {
					params = string(pb)
				}
			}
		} else {
			name, _ = t["name"].(string)
			desc, _ = t["description"].(string)
		}
		if name == "" {
			continue
		}
		b.WriteString("\n- " + name)
		if desc != "" {
			b.WriteString(": " + desc)
		}
		if params != "" {
			b.WriteString(" Parameters: " + params)
		}
	}
	b.WriteString("\nDSML format (use the exact fullwidth delimiters):")
	b.WriteString("\n<｜DSML｜calls>\n<｜DSML｜invoke name=\"TOOL_NAME\">\n<｜DSML｜parameter name=\"PARAM_NAME\" string=\"true\">\nVALUE\n</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜calls>")
	b.WriteString("\nRules: keep parameter names/values exact; multiple invokes and parameters allowed; text outside DSML blocks is the assistant reply.")
	return b.String()
}

// dsmlToOpenAIToolCalls converts normalized DSML calls to OpenAI tool_calls.
func dsmlToOpenAIToolCalls(calls []ds.ToolCall) []interface{} {
	out := make([]interface{}, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]interface{}{
			"id":   c.ID,
			"type": "function",
			"function": map[string]interface{}{
				"name":      c.Name,
				"arguments": c.ArgumentsJSON(),
			},
		})
	}
	return out
}

func main() {
	repoDir := os.Getenv("REPO_DIR")
	if repoDir == "" {
		repoDir = "."
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	apiKey := os.Getenv("GATEWAY_API_KEY")

	client := ds.New(repoDir)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := client.EnsureLogin(ctx); err != nil {
		log.Printf("warning: initial login failed (%v); will retry on first request", err)
	} else {
		log.Printf("deepseek auth OK")
	}

	mux := http.NewServeMux()

	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if apiKey != "" && r.Header.Get("Authorization") != "Bearer "+apiKey {
				errJSON(w, 401, "Invalid API key")
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /v1/models", auth(func(w http.ResponseWriter, r *http.Request) {
		type pub struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		}
		data := make([]pub, 0, len(models))
		for _, m := range models {
			data = append(data, pub{m.ID, m.Object, m.Created, m.OwnedBy})
		}
		writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
	}))

	mux.HandleFunc("POST /v1/sessions", auth(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		s, err := client.CreateSession(ctx)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{
			"id": s.ID, "object": "session", "seq_id": s.SeqID,
			"model_type": s.ModelType, "created": s.Inserted,
		})
	}))

	mux.HandleFunc("GET /v1/sessions", auth(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		list, err := client.ListSessions(ctx)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"object": "list", "data": list})
	}))

	mux.HandleFunc("DELETE /v1/sessions/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		id := r.PathValue("id")
		if err := client.DeleteSession(ctx, id); err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]interface{}{"id": id, "object": "session", "deleted": true})
	}))

	mux.HandleFunc("POST /v1/chat/completions", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model           string                   `json:"model"`
			Messages        []map[string]interface{} `json:"messages"`
			Tools           []map[string]interface{} `json:"tools"`
			Stream          bool                     `json:"stream"`
			SessionID       string                   `json:"session_id"`
			ThinkingEnabled *bool                    `json:"thinking_enabled"`
			SearchEnabled   *bool                    `json:"search_enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20)).Decode(&req); err != nil {
			errJSON(w, 400, "bad request: "+err.Error())
			return
		}
		model := resolveModel(req.Model)
		fullPrompt := appendToolsSection(messagesToPrompt(req.Messages), req.Tools)
		if strings.TrimSpace(fullPrompt) == "" {
			errJSON(w, 400, "messages is required")
			return
		}

		thinking := model.Thinking
		if req.ThinkingEnabled != nil {
			thinking = *req.ThinkingEnabled
		}
		search := false
		if req.SearchEnabled != nil {
			search = *req.SearchEnabled
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		prompt := fullPrompt
		sessionID := req.SessionID
		reused := false
		switch {
		case !isAutoSessionID(sessionID):
			// Explicit session_id: honor as-is (documented multi-turn usage).
		case !sessionAffinityEnabled():
			s, err := client.CreateSession(ctx)
			if err != nil {
				errJSON(w, 502, err.Error())
				return
			}
			sessionID = s.ID
		default:
			cur := normalizeAffinityMessages(req.Messages)
			if match := findAffinity(cur); match != nil && len(cur) >= len(match.Messages) {
				sessionID = match.SessionID
				reused = true
				tail := req.Messages[len(match.Messages):]
				if len(tail) == 0 && len(req.Messages) > 0 {
					// Identical retry: resend the last turn.
					tail = req.Messages[len(req.Messages)-1:]
				}
				if len(tail) > 0 {
					prompt = appendToolsSection(messagesToPrompt(tail), req.Tools)
					if strings.TrimSpace(prompt) == "" {
						prompt = fullPrompt
					}
				}
			} else {
				s, err := client.CreateSession(ctx)
				if err != nil {
					errJSON(w, 502, err.Error())
					return
				}
				sessionID = s.ID
			}
		}
		if reused {
			log.Printf("affinity: reuse web session %s (msgs %d, tail prompt len %d)", shortID(sessionID), len(req.Messages), len(prompt))
		} else if isAutoSessionID(req.SessionID) && sessionAffinityEnabled() {
			log.Printf("affinity: new web session %s (msgs %d)", shortID(sessionID), len(req.Messages))
		}
		opt := ds.CompletionOptions{
			SessionID: sessionID, Prompt: prompt, ModelType: model.ModelType,
			ThinkingEnabled: thinking, SearchEnabled: search,
		}
		created := time.Now().Unix()
		chatID := "chatcmpl-" + shortID(sessionID)
		if dsmlDebugEnabled() {
			log.Printf("[dsml] prompt %s: len=%d tools=%d preview=%q",
				chatID, len(prompt), len(req.Tools), previewRunes(prompt, 1000))
		}

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			fl, _ := w.(http.Flusher)
			base := map[string]interface{}{
				"id": chatID, "object": "chat.completion.chunk",
				"created": created, "model": model.ID, "session_id": sessionID,
			}
			chunk := func(delta map[string]interface{}, finish string) {
				c := map[string]interface{}{}
				for k, v := range base {
					c[k] = v
				}
				choice := map[string]interface{}{"index": 0, "delta": delta, "finish_reason": nil}
				if finish != "" {
					choice["finish_reason"] = finish
				}
				c["choices"] = []interface{}{choice}
				b, _ := json.Marshal(c)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if fl != nil {
					fl.Flush()
				}
			}
			// DSML-aware streaming: raw DSML bytes are never forwarded as
			// content. Safe text flows incrementally; tool_calls are emitted
			// once the DeepSeek stream finishes.
			// When DSML_ENABLED=0 the filter is bypassed and deltas pass through
			// verbatim (no tool_calls ever emitted).
			var filter ds.StreamFilter
			dsmlPassthrough := !dsmlParsingEnabled()
			toolChunk := func(index int, tc ds.ToolCall) {
				chunk(map[string]interface{}{
					"tool_calls": []interface{}{map[string]interface{}{
						"index": index,
						"id":    tc.ID,
						"type":  "function",
						"function": map[string]interface{}{
							"name":      tc.Name,
							"arguments": tc.ArgumentsJSON(),
						},
					}},
				}, "")
			}
			_, err := client.StreamCompletion(ctx, opt, func(e ds.Event) {
				switch e.Type {
				case "text":
					if dsmlPassthrough {
						chunk(map[string]interface{}{"content": e.Delta}, "")
						break
					}
					if safe := filter.Write(e.Delta); safe != "" {
						chunk(map[string]interface{}{"content": safe}, "")
					}
				case "thinking":
					if thinking {
						chunk(map[string]interface{}{"reasoning_content": e.Delta}, "")
					}
				}
				if fl != nil {
					fl.Flush()
				}
			})
			if err != nil {
				chunk(map[string]interface{}{}, "error")
				fmt.Fprintf(w, "data: [DONE]\n\n")
				return
			}
			rest, calls := filter.Flush()
			if dsmlPassthrough {
				calls = nil
				rest = ""
				if dsmlDebugEnabled() {
					log.Printf("[dsml] stream %s: parser disabled, passthrough", chatID)
				}
			} else if dsmlDebugEnabled() {
				// Re-parse the buffered raw text for the debug summary.
				logDSMLRaw("stream "+chatID, filter.Raw(), ds.ParseDSML(filter.Raw()))
			}
			if rest != "" {
				chunk(map[string]interface{}{"content": rest}, "")
			}
			for i, tc := range calls {
				toolChunk(i, tc)
			}
			if len(calls) > 0 {
				chunk(map[string]interface{}{}, "tool_calls")
			} else {
				chunk(map[string]interface{}{}, "stop")
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			upsertAffinity(sessionID, req.Messages, fullPrompt)
			return
		}

		text, reasoning, _, err := client.Complete(ctx, opt)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		content, toolCalls, finish := adaptDSML(text)
		if dsmlDebugEnabled() {
			if dsmlParsingEnabled() {
				logDSMLRaw("non-stream "+chatID, text, ds.ParseDSML(text))
			} else {
				log.Printf("[dsml] non-stream %s: parser disabled, passthrough len=%d", chatID, len(text))
			}
		}
		msg := map[string]interface{}{"role": "assistant", "content": content}
		if thinking && reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		upsertAffinity(sessionID, req.Messages, fullPrompt)
		writeJSON(w, 200, map[string]interface{}{
			"id": chatID, "object": "chat.completion", "created": created, "model": model.ID,
			"session_id": sessionID,
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "message": msg, "finish_reason": finish,
			}},
			"usage": map[string]int{"prompt_tokens": -1, "completion_tokens": -1, "total_tokens": -1},
		})
	}))

	mux.HandleFunc("POST /v1/completions", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string      `json:"model"`
			Prompt    interface{} `json:"prompt"`
			Stream    bool        `json:"stream"`
			SessionID string      `json:"session_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20)).Decode(&req); err != nil {
			errJSON(w, 400, "bad request: "+err.Error())
			return
		}
		model := resolveModel(req.Model)
		var fullPrompt string
		switch t := req.Prompt.(type) {
		case string:
			fullPrompt = t
		default:
			fullPrompt = fmt.Sprint(req.Prompt)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		prompt := fullPrompt
		sessionID := req.SessionID
		switch {
		case !isAutoSessionID(sessionID):
			// Explicit session_id wins.
		case !sessionAffinityEnabled():
			s, err := client.CreateSession(ctx)
			if err != nil {
				errJSON(w, 502, err.Error())
				return
			}
			sessionID = s.ID
		default:
			if match := findAffinityByPrompt(fullPrompt); match != nil {
				sessionID = match.SessionID
				if tail := strings.TrimPrefix(fullPrompt, match.Prompt); strings.TrimSpace(tail) != "" {
					prompt = tail
				}
				log.Printf("affinity: reuse web session %s (legacy completions)", shortID(sessionID))
			} else {
				s, err := client.CreateSession(ctx)
				if err != nil {
					errJSON(w, 502, err.Error())
					return
				}
				sessionID = s.ID
			}
		}
		opt := ds.CompletionOptions{SessionID: sessionID, Prompt: prompt, ModelType: model.ModelType}
		created := time.Now().Unix()
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			fl, _ := w.(http.Flusher)
			base := map[string]interface{}{
				"id": "cmpl-" + shortID(sessionID), "object": "text_completion",
				"created": created, "model": model.ID,
			}
			send := func(text, finish string) {
				c := map[string]interface{}{}
				for k, v := range base {
					c[k] = v
				}
				choice := map[string]interface{}{"text": text, "index": 0, "finish_reason": nil}
				if finish != "" {
					choice["finish_reason"] = finish
				}
				c["choices"] = []interface{}{choice}
				b, _ := json.Marshal(c)
				fmt.Fprintf(w, "data: %s\n\n", b)
				if fl != nil {
					fl.Flush()
				}
			}
			_, err := client.StreamCompletion(ctx, opt, func(e ds.Event) {
				if e.Type == "text" {
					send(e.Delta, "")
				}
			})
			if err == nil {
				send("", "stop")
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			upsertAffinityPrompt(sessionID, fullPrompt)
			return
		}
		text, _, _, err := client.Complete(ctx, opt)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		upsertAffinityPrompt(sessionID, fullPrompt)
		writeJSON(w, 200, map[string]interface{}{
			"id": "cmpl-" + shortID(sessionID), "object": "text_completion",
			"created": created, "model": model.ID, "session_id": sessionID,
			"choices": []interface{}{map[string]interface{}{
				"text": text, "index": 0, "finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": -1, "completion_tokens": -1, "total_tokens": -1},
		})
	}))

	log.Printf("chat2api (go) listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}

func shortID(s string) string {
	s = strings.ReplaceAll(s, "-", "")
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
