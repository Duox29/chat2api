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
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"chat2api/ds"
)

type Model struct {
	ID         string `json:"id"`
	Object     string `json:"object"`
	Created    int64  `json:"created"`
	OwnedBy    string `json:"owned_by"`
	ModelType  string `json:"-"`
	Thinking   bool   `json:"-"`
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

// messagesToPrompt flattens OpenAI messages[] into one DeepSeek prompt.
func messagesToPrompt(messages []map[string]interface{}) string {
	if len(messages) == 0 {
		return ""
	}
	str := func(v interface{}) string {
		switch t := v.(type) {
		case string:
			return t
		case []interface{}:
			var b strings.Builder
			for _, p := range t {
				pm, _ := p.(map[string]interface{})
				if pm["type"] == "text" {
					if s, ok := pm["text"].(string); ok {
						b.WriteString(s + "\n")
					}
				}
			}
			return strings.TrimRight(b.String(), "\n")
		}
		return fmt.Sprint(v)
	}
	if len(messages) == 1 {
		return str(messages[0]["content"])
	}
	var b strings.Builder
	for _, m := range messages {
		role, _ := m["role"].(string)
		var label string
		switch role {
		case "assistant":
			label = "Assistant"
		case "system":
			label = "System"
		default:
			label = "User"
		}
		b.WriteString(label + ": " + str(m["content"]) + "\n\n")
	}
	return strings.TrimSpace(b.String())
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
		prompt := messagesToPrompt(req.Messages)
		if strings.TrimSpace(prompt) == "" {
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

		sessionID := req.SessionID
		if sessionID == "" {
			s, err := client.CreateSession(ctx)
			if err != nil {
				errJSON(w, 502, err.Error())
				return
			}
			sessionID = s.ID
		}
		opt := ds.CompletionOptions{
			SessionID: sessionID, Prompt: prompt, ModelType: model.ModelType,
			ThinkingEnabled: thinking, SearchEnabled: search,
		}
		created := time.Now().Unix()
		chatID := "chatcmpl-" + shortID(sessionID)

		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			fl, _ := w.(http.Flusher)
			base := map[string]interface{}{
				"id": chatID, "object": "chat.completion.chunk",
				"created": created, "model": model.ID,
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
			_, err := client.StreamCompletion(ctx, opt, func(e ds.Event) {
				switch e.Type {
				case "text":
					chunk(map[string]interface{}{"content": e.Delta}, "")
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
			} else {
				chunk(map[string]interface{}{}, "stop")
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}

		text, reasoning, _, err := client.Complete(ctx, opt)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
		msg := map[string]interface{}{"role": "assistant", "content": text}
		if thinking && reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
		writeJSON(w, 200, map[string]interface{}{
			"id": chatID, "object": "chat.completion", "created": created, "model": model.ID,
			"session_id": sessionID,
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "message": msg, "finish_reason": "stop",
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
		var prompt string
		switch t := req.Prompt.(type) {
		case string:
			prompt = t
		default:
			prompt = fmt.Sprint(req.Prompt)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		sessionID := req.SessionID
		if sessionID == "" {
			s, err := client.CreateSession(ctx)
			if err != nil {
				errJSON(w, 502, err.Error())
				return
			}
			sessionID = s.ID
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
			return
		}
		text, _, _, err := client.Complete(ctx, opt)
		if err != nil {
			errJSON(w, 502, err.Error())
			return
		}
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
