package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"chat2api/ds"
)

func TestMessagesToPromptToolRoundTrip(t *testing.T) {
	msgs := []map[string]interface{}{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "list files"},
		{"role": "assistant", "content": "",
			"tool_calls": []interface{}{
				map[string]interface{}{"id": "call_1",
					"function": map[string]interface{}{"name": "bash", "arguments": `{"command":"pwd"}`}},
			}},
		{"role": "tool", "content": "/home/x", "name": "bash", "tool_call_id": "call_1"},
	}
	p := messagesToPrompt(msgs)
	for _, want := range []string{"System: sys", "User: list files", "bash", "call_1", "/home/x"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestAppendToolsSection(t *testing.T) {
	base := "User: hi"
	tools := []map[string]interface{}{
		{"function": map[string]interface{}{
			"name": "bash", "description": "run shell",
			"parameters": map[string]interface{}{"type": "object"},
		}},
	}
	p := appendToolsSection(base, tools)
	if !strings.Contains(p, "bash") || !strings.Contains(p, "DSML") || !strings.Contains(p, "｜") {
		t.Fatalf("tools section missing:\n%s", p)
	}
	if got := appendToolsSection(base, nil); got != base {
		t.Fatalf("nil tools must not alter prompt")
	}
}

func TestDsmlToOpenAIToolCalls(t *testing.T) {
	parsed := ds.ParseDSML("<｜DSML｜calls><｜DSML｜invoke name=\"bash\">" +
		"<｜DSML｜parameter name=\"command\">pwd</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜calls>")
	if len(parsed.Calls) != 1 {
		t.Fatal(parsed)
	}
	tcs := dsmlToOpenAIToolCalls(parsed.Calls)
	if len(tcs) != 1 {
		t.Fatal(tcs)
	}
	m := tcs[0].(map[string]interface{})
	fn := m["function"].(map[string]interface{})
	if fn["name"] != "bash" {
		t.Fatal(fn)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if args["command"] != "pwd" {
		t.Fatal(args)
	}
}

func TestDsmlDebugGate(t *testing.T) {
	old, had := os.LookupEnv("DSML_DEBUG")
	defer func() {
		if had {
			os.Setenv("DSML_DEBUG", old)
		} else {
			os.Unsetenv("DSML_DEBUG")
		}
	}()
	os.Unsetenv("DSML_DEBUG")
	if dsmlDebugEnabled() {
		t.Fatal("debug must be off by default")
	}
	for _, v := range []string{"1", "true", "TRUE", "raw", "on", " yes "} {
		os.Setenv("DSML_DEBUG", v)
		if !dsmlDebugEnabled() {
			t.Fatalf("debug must be on for %q", v)
		}
	}
	os.Setenv("DSML_DEBUG", "0")
	if dsmlDebugEnabled() {
		t.Fatal("debug must be off for 0")
	}
}

func TestPreviewRunesKeepsBarsIntact(t *testing.T) {
	if got := previewRunes("abc", 10); got != "abc" {
		t.Fatalf("short string altered: %q", got)
	}
	// "｜" is 1 rune / 3 bytes: truncating to 2 runes must not split it.
	got := previewRunes("a｜bcd", 2)
	if got != "a｜…[3 more runes]" {
		t.Fatalf("rune-unsafe truncation: %q", got)
	}
	if !strings.HasPrefix(previewRunes("<｜｜DSML｜｜ calls>", 4), "<｜｜D") {
		t.Fatalf("unexpected preview: %q", previewRunes("<｜｜DSML｜｜ calls>", 4))
	}
}

func TestStreamFilterRawAccessor(t *testing.T) {
	var f ds.StreamFilter
	raw := "<｜｜DSML｜｜calls>" + "<｜｜DSML｜｜invoke name=\"bash\">x</｜｜DSML｜｜invoke>" + "</｜｜DSML｜｜calls>"
	f.Write(raw)
	if f.Raw() != raw {
		t.Fatalf("Raw() mismatch: %q", f.Raw())
	}
	// logDSMLRaw must be a safe no-op when the flag is off.
	os.Unsetenv("DSML_DEBUG")
	logDSMLRaw("test", raw, ds.ParseDSML(raw))
}

func TestDsmlEnabledToggle(t *testing.T) {
	old, had := os.LookupEnv("DSML_ENABLED")
	defer func() {
		if had {
			os.Setenv("DSML_ENABLED", old)
		} else {
			os.Unsetenv("DSML_ENABLED")
		}
	}()
	raw := "<｜DSML｜calls><｜DSML｜invoke name=\"bash\">" +
		"<｜DSML｜parameter name=\"command\">pwd</｜DSML｜parameter>" +
		"</｜DSML｜invoke></｜DSML｜calls>"

	// Default (unset) and truthy values: parser ON.
	for _, v := range []string{"", "1", "true", "yes", "on"} {
		if v == "" {
			os.Unsetenv("DSML_ENABLED")
		} else {
			os.Setenv("DSML_ENABLED", v)
		}
		if !dsmlParsingEnabled() {
			t.Fatalf("parser must be on for %q", v)
		}
		content, tcs, finish := adaptDSML(raw)
		if content != "pwd" && content != "" {
			// content is the clean text (DSML stripped); must not equal raw.
			t.Fatalf("parser on: DSML not stripped for %q: %q", v, content)
		}
		if len(tcs) != 1 || finish != "tool_calls" {
			t.Fatalf("parser on: expected 1 tool_call for %q", v)
		}
	}

	// Falsy values: parser OFF → verbatim passthrough.
	for _, v := range []string{"0", "false", "FALSE", "no", "off"} {
		os.Setenv("DSML_ENABLED", v)
		if dsmlParsingEnabled() {
			t.Fatalf("parser must be off for %q", v)
		}
		content, tcs, finish := adaptDSML(raw)
		if content != raw || tcs != nil || finish != "stop" {
			t.Fatalf("parser off: expected verbatim passthrough for %q", v)
		}
	}

	// Plain text is unaffected in both modes.
	for _, v := range []string{"", "0"} {
		if v == "" {
			os.Unsetenv("DSML_ENABLED")
		} else {
			os.Setenv("DSML_ENABLED", v)
		}
		if c, tcs, f := adaptDSML("Hello"); c != "Hello" || tcs != nil || f != "stop" {
			t.Fatalf("plain text altered (mode %q): %q %v %q", v, c, tcs, f)
		}
	}
}
