package ds

import (
	"encoding/json"
	"strings"
	"testing"
)

const fw = "｜" // U+FF5C, must match dsml.go fwBar

func dsmlOpen(tag, attrs string) string {
	return "<" + fw + "DSML" + fw + tag + attrs + ">"
}

func dsmlClose(tag string) string {
	return "</" + fw + "DSML" + fw + tag + ">"
}

func wrapCalls(inner string) string {
	return dsmlOpen("calls", "") + "\n" + inner + "\n" + dsmlClose("calls")
}

func TestParseOneInvoke(t *testing.T) {
	raw := wrapCalls(dsmlOpen("invoke", ` name="bash"`) + "\n" +
		dsmlOpen("parameter", ` name="command" string="true"`) + "\nls -la /home/duox/IdeaProjects\n" + dsmlClose("parameter") + "\n" +
		dsmlClose("invoke"))
	res := ParseDSML(raw)
	if !res.HasDSML {
		t.Fatal("expected HasDSML")
	}
	if len(res.Calls) != 1 {
		t.Fatalf("got %d calls: %+v", len(res.Calls), res.Calls)
	}
	c := res.Calls[0]
	if c.Name != "bash" {
		t.Fatalf("name=%q", c.Name)
	}
	if c.Arguments["command"] != "ls -la /home/duox/IdeaProjects" {
		t.Fatalf("args=%v", c.Arguments)
	}
	if res.CleanText != "" {
		t.Fatalf("clean=%q", res.CleanText)
	}
	// arguments must marshal to valid JSON object
	var m map[string]string
	if err := json.Unmarshal([]byte(c.ArgumentsJSON()), &m); err != nil {
		t.Fatal(err)
	}
	if m["command"] != "ls -la /home/duox/IdeaProjects" {
		t.Fatalf("json args=%v", m)
	}
}

func TestParseMultipleInvokes(t *testing.T) {
	raw := wrapCalls(
		dsmlOpen("invoke", ` name="bash"`) +
			dsmlOpen("parameter", ` name="command" string="true"`) + "pwd" + dsmlClose("parameter") +
			dsmlClose("invoke") + "\n" +
			dsmlOpen("invoke", ` name="glob"`) +
			dsmlOpen("parameter", ` name="pattern" string="true"`) + "/package.json" + dsmlClose("parameter") +
			dsmlClose("invoke"))
	res := ParseDSML(raw)
	if len(res.Calls) != 2 {
		t.Fatalf("got %+v", res.Calls)
	}
	if res.Calls[0].Name != "bash" || res.Calls[1].Name != "glob" {
		t.Fatalf("names=%q %q", res.Calls[0].Name, res.Calls[1].Name)
	}
}

func TestParseMultipleParams(t *testing.T) {
	raw := wrapCalls(dsmlOpen("invoke", ` name="edit"`) +
		dsmlOpen("parameter", ` name="path"`) + "a.txt" + dsmlClose("parameter") +
		dsmlOpen("parameter", ` name="old"`) + "x" + dsmlClose("parameter") +
		dsmlOpen("parameter", ` name="new"`) + "y" + dsmlClose("parameter") +
		dsmlClose("invoke"))
	res := ParseDSML(raw)
	if len(res.Calls) != 1 {
		t.Fatal(res.Calls)
	}
	if len(res.Calls[0].Arguments) != 3 {
		t.Fatalf("args=%v", res.Calls[0].Arguments)
	}
}

func TestEmptyAndMultilineParam(t *testing.T) {
	raw := wrapCalls(dsmlOpen("invoke", ` name="bash"`) +
		dsmlOpen("parameter", ` name="command"`) + "" + dsmlClose("parameter") +
		dsmlClose("invoke"))
	if got := ParseDSML(raw).Calls[0].Arguments["command"]; got != "" {
		t.Fatalf("empty=%q", got)
	}
	multi := "line1\nline2\n  indented\nline4"
	raw2 := wrapCalls(dsmlOpen("invoke", ` name="edit"`) +
		dsmlOpen("parameter", ` name="content"`) + "\n" + multi + "\n" + dsmlClose("parameter") +
		dsmlClose("invoke"))
	if got := ParseDSML(raw2).Calls[0].Arguments["content"]; got != multi {
		t.Fatalf("multi=%q want %q", got, multi)
	}
}

func TestNormalTextOnly(t *testing.T) {
	res := ParseDSML("Hello, how are you?")
	if res.HasDSML || len(res.Calls) != 0 || res.CleanText != "Hello, how are you?" {
		t.Fatalf("%+v", res)
	}
}

func TestTextPlusDSML(t *testing.T) {
	dsml := wrapCalls(dsmlOpen("invoke", ` name="bash"`) +
		dsmlOpen("parameter", ` name="command"`) + "pwd" + dsmlClose("parameter") +
		dsmlClose("invoke"))
	res := ParseDSML("I will inspect the project.\n\n" + dsml)
	if len(res.Calls) != 1 || res.CleanText != "I will inspect the project." {
		t.Fatalf("%+v", res)
	}
	res2 := ParseDSML(dsml + "\nDone.")
	if len(res2.Calls) != 1 || res2.CleanText != "Done." {
		t.Fatalf("%+v", res2)
	}
}

func TestMultipleBlocks(t *testing.T) {
	b1 := wrapCalls(dsmlOpen("invoke", ` name="bash"`) + dsmlOpen("parameter", ` name="command"`) + "a" + dsmlClose("parameter") + dsmlClose("invoke"))
	b2 := wrapCalls(dsmlOpen("invoke", ` name="glob"`) + dsmlOpen("parameter", ` name="pattern"`) + "b" + dsmlClose("parameter") + dsmlClose("invoke"))
	res := ParseDSML("start\n" + b1 + "\nmiddle\n" + b2 + "\nend")
	if len(res.Calls) != 2 {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(res.CleanText, "start") || !strings.Contains(res.CleanText, "middle") || !strings.Contains(res.CleanText, "end") {
		t.Fatalf("clean=%q", res.CleanText)
	}
	if strings.Contains(res.CleanText, "DSML") {
		t.Fatalf("DSML leaked: %q", res.CleanText)
	}
}

func TestASCIIFallback(t *testing.T) {
	raw := "<|DSML|calls><|DSML|invoke name=\"bash\"><|DSML|parameter name=\"command\">pwd</|DSML|parameter></|DSML|invoke></|DSML|calls>"
	res := ParseDSML(raw)
	if len(res.Calls) != 1 || res.Calls[0].Name != "bash" {
		t.Fatalf("%+v", res)
	}
}

func TestInvalidInputsFailSafe(t *testing.T) {
	cases := map[string]string{
		"missing calls close":  dsmlOpen("calls", "") + dsmlOpen("invoke", ` name="bash"`) + "x",
		"missing invoke close": wrapCalls(dsmlOpen("invoke", ` name="bash"`) + dsmlOpen("parameter", ` name="c"`) + "pwd" + dsmlClose("parameter")),
		"invoke without name":  wrapCalls(dsmlOpen("invoke", "") + dsmlClose("invoke")),
		"nested calls":         dsmlOpen("calls", "") + dsmlOpen("calls", "") + dsmlClose("calls"),
		"ordinary angle text":  "a < b and c > d, use <foo> tags",
	}
	for name, raw := range cases {
		res := ParseDSML(raw)
		if len(res.Calls) != 0 {
			t.Fatalf("%s: expected 0 calls, got %+v", name, res.Calls)
		}
	}
	// Parameter without a name is corruption, not a no-arg call: the whole
	// invoke is dropped fail-safe (forwarding arguments={} makes downstream
	// validators fail, e.g. opencode `read`: `Received arguments: {}`).
	rawBadParam := wrapCalls(dsmlOpen("invoke", ` name="bash"`) + dsmlOpen("parameter", ``) + "x" + dsmlClose("parameter") + dsmlClose("invoke"))
	if res := ParseDSML(rawBadParam); len(res.Calls) != 0 {
		t.Fatalf("nameless parameter must drop the invoke: %+v", res)
	}
	// missing parameter close: value boundary unknown -> drop invoke, no
	// hallucinated empty-args call.
	raw := wrapCalls(dsmlOpen("invoke", ` name="bash"`) + dsmlOpen("parameter", ` name="c"`) + "pwd" + dsmlClose("invoke"))
	res := ParseDSML(raw)
	if len(res.Calls) != 0 {
		t.Fatalf("missing param close must yield no calls: %+v", res)
	}
}

func TestUnknownToolStillReturned(t *testing.T) {
	raw := wrapCalls(dsmlOpen("invoke", ` name="frobnicate"`) + dsmlOpen("parameter", ` name="x"`) + "1" + dsmlClose("parameter") + dsmlClose("invoke"))
	res := ParseDSML(raw)
	if len(res.Calls) != 1 || res.Calls[0].Name != "frobnicate" {
		t.Fatalf("%+v", res)
	}
}

func TestUnicodeDelimiters(t *testing.T) {
	if len(fw) == 0 {
		t.Fatal("fw empty")
	}
	raw := "<" + fw + "DSML" + fw + "calls>" +
		"<" + fw + "DSML" + fw + "invoke name=\"read\">" +
		"<" + fw + "DSML" + fw + "parameter name=\"path\">a.txt</" + fw + "DSML" + fw + "parameter>" +
		"</" + fw + "DSML" + fw + "invoke></" + fw + "DSML" + fw + "calls>"
	res := ParseDSML(raw)
	if len(res.Calls) != 1 || res.Calls[0].Arguments["path"] != "a.txt" {
		t.Fatalf("%+v", res)
	}
	// ASCII pipe must not be confused: fullwidth parse must work even when
	// ASCII variant absent, and vice versa (covered elsewhere).
}

func TestStreamingSplitBoundaries(t *testing.T) {
	full := "I will inspect.\n" + wrapCalls(dsmlOpen("invoke", ` name="bash"`)+
		dsmlOpen("parameter", ` name="command" string="true"`)+"\nls -la\n"+dsmlClose("parameter")+
		dsmlClose("invoke")) + "\nDone."
	// Split into every possible small chunk pattern: 1..7 byte chunks.
	for _, size := range []int{1, 2, 3, 5, 7} {
		var f StreamFilter
		var safe strings.Builder
		for i := 0; i < len(full); i += size {
			end := i + size
			if end > len(full) {
				end = len(full)
			}
			safe.WriteString(f.Write(full[i:end]))
		}
		rest, calls := f.Flush()
		safe.WriteString(rest)
		if len(calls) != 1 || calls[0].Name != "bash" || calls[0].Arguments["command"] != "ls -la" {
			t.Fatalf("size=%d calls=%+v", size, calls)
		}
		if strings.Contains(safe.String(), "DSML") {
			t.Fatalf("size=%d DSML leaked: %q", size, safe.String())
		}
		if !strings.Contains(safe.String(), "I will inspect.") || !strings.Contains(safe.String(), "Done.") {
			t.Fatalf("size=%d safe=%q", size, safe.String())
		}
	}
}

func TestStreamingExampleFromSpec(t *testing.T) {
	chunks := []string{
		"<" + fw + "DSML" + fw + "cal",
		"ls><" + fw + "DSML" + fw + "invoke name=\"bash\">",
		"<" + fw + "DSML" + fw + "parameter name=\"command\" string=\"true\">",
		"ls -la",
		"</" + fw + "DSML" + fw + "parameter>",
		"</" + fw + "DSML" + fw + "invoke></" + fw + "DSML" + fw + "calls>",
	}
	var f StreamFilter
	safe := ""
	for _, c := range chunks {
		safe += f.Write(c)
	}
	if strings.Contains(safe, "ls -la") {
		t.Fatalf("DSML content leaked early: %q", safe)
	}
	rest, calls := f.Flush()
	safe += rest
	if len(calls) != 1 || calls[0].Arguments["command"] != "ls -la" {
		t.Fatalf("calls=%+v safe=%q", calls, safe)
	}
}

func TestStreamingNormalTextPassesThrough(t *testing.T) {
	var f StreamFilter
	out := f.Write("Hello, ") + f.Write("how are you?")
	rest, calls := f.Flush()
	out += rest
	if out != "Hello, how are you?" || len(calls) != 0 {
		t.Fatalf("out=%q calls=%v", out, calls)
	}
}

func TestWhitespaceTolerance(t *testing.T) {
	raw := "<" + fw + "DSML" + fw + "calls  >\n <" + fw + "DSML" + fw + "invoke   name=\"bash\"  >\n<" + fw + "DSML" + fw + "parameter  name=\"command\"  >pwd</" + fw + "DSML" + fw + "parameter  >\n</" + fw + "DSML" + fw + "invoke  >\n</" + fw + "DSML" + fw + "calls>"
	res := ParseDSML(raw)
	if len(res.Calls) != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestEscapedChars(t *testing.T) {
	raw := wrapCalls(dsmlOpen("invoke", ` name="bash"`) +
		dsmlOpen("parameter", ` name="command"`) + "echo &lt;hi&gt; &amp;" + dsmlClose("parameter") +
		dsmlClose("invoke"))
	got := ParseDSML(raw).Calls[0].Arguments["command"]
	if got != "echo <hi> &" {
		t.Fatalf("got %q", got)
	}
}

func TestDoubleBarVariant(t *testing.T) {
	// Exact shape pasted from the live session: doubled fullwidth bars and a
	// space before the tag name.
	raw := "<｜｜DSML｜｜ calls>\n" +
		"<｜｜DSML｜｜ invoke name=\"read\">\n" +
		"<｜｜DSML｜｜ parameter name=\"i\" string=\"true\">Reading main Go entrypoint</｜｜DSML｜｜ parameter>\n" +
		"<｜｜DSML｜｜ parameter name=\"path\" string=\"true\">main.go</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"<｜｜DSML｜｜ invoke name=\"read\">\n" +
		"<｜｜DSML｜｜ parameter name=\"path\" string=\"true\">ds/client.go</｜｜DSML｜｜ parameter>\n" +
		"</｜｜DSML｜｜ invoke>\n" +
		"</｜｜DSML｜｜ calls>"
	res := ParseDSML(raw)
	if len(res.Calls) != 2 {
		t.Fatalf("got %+v", res.Calls)
	}
	if res.Calls[0].Name != "read" || res.Calls[0].Arguments["path"] != "main.go" ||
		res.Calls[0].Arguments["i"] != "Reading main Go entrypoint" {
		t.Fatalf("call0=%+v", res.Calls[0])
	}
	if res.Calls[1].Arguments["path"] != "ds/client.go" {
		t.Fatalf("call1=%+v", res.Calls[1])
	}
	if res.CleanText != "" {
		t.Fatalf("clean=%q", res.CleanText)
	}
	// Streaming split across the doubled bars must not leak.
	var f StreamFilter
	var sb strings.Builder
	for i := 0; i < len(raw); i += 2 {
		end := i + 2
		if end > len(raw) {
			end = len(raw)
		}
		sb.WriteString(f.Write(raw[i:end]))
	}
	rest, calls := f.Flush()
	sb.WriteString(rest)
	if len(calls) != 2 {
		t.Fatalf("stream calls=%+v", calls)
	}
	if strings.Contains(sb.String(), "DSML") {
		t.Fatalf("DSML leaked: %q", sb.String())
	}
}

func TestStreamFilterNeverDuplicates(t *testing.T) {
	// Verbatim repro: leading blank lines + trailing newline before a block.
	// Write releases the raw prefix verbatim; Flush must return only the
	// remainder, not the whole cleaned text again.
	block := wrapCalls(dsmlOpen("invoke", ` name="bash"`)+
		dsmlOpen("parameter", ` name="command" string="true"`)+"pwd"+dsmlClose("parameter")+
		dsmlClose("invoke"))
	delta := "\n\nHello there.\n" + block
	var f StreamFilter
	safe := f.Write(delta)
	rest, calls := f.Flush()
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	if total := safe + rest; total != "\n\nHello there.\n" {
		t.Fatalf("duplicated/corrupt total=%q (safe=%q rest=%q)", total, safe, rest)
	}
}

func TestStreamFilterLeadingWhitespaceNoDSML(t *testing.T) {
	// No DSML at all: TrimSpace in CleanText must not cause re-emission.
	var f StreamFilter
	safe := f.Write("\n\nHello")
	rest, _ := f.Flush()
	if total := safe + rest; total != "\n\nHello" {
		t.Fatalf("total=%q (safe=%q rest=%q)", total, safe, rest)
	}
}

func TestStreamFilterChunkedEqualsCleanText(t *testing.T) {
	// Without leading/trailing whitespace, chunked streaming must reassemble
	// to exactly ParseDSML(full).CleanText — no duplication, no loss.
	full := "I will inspect.\n" + wrapCalls(dsmlOpen("invoke", ` name="bash"`)+
		dsmlOpen("parameter", ` name="command" string="true"`)+"\nls -la\n"+dsmlClose("parameter")+
		dsmlClose("invoke")) + "\nDone."
	want := ParseDSML(full).CleanText
	for _, size := range []int{1, 2, 3, 5, 7, 64} {
		var f StreamFilter
		var sb strings.Builder
		for i := 0; i < len(full); i += size {
			end := i + size
			if end > len(full) {
				end = len(full)
			}
			sb.WriteString(f.Write(full[i:end]))
		}
		rest, calls := f.Flush()
		sb.WriteString(rest)
		if len(calls) != 1 {
			t.Fatalf("size=%d calls=%+v", size, calls)
		}
		if sb.String() != want {
			t.Fatalf("size=%d total=%q want=%q", size, sb.String(), want)
		}
	}
}

func TestMixedSingleDoubleBars(t *testing.T) {
	raw := "<｜DSML｜calls>" +
		"<｜｜DSML｜｜invoke name=\"bash\">" +
		"<｜DSML｜parameter name=\"command\">pwd</｜｜DSML｜｜parameter>" +
		"</｜DSML｜invoke></｜｜DSML｜｜calls>"
	res := ParseDSML(raw)
	if len(res.Calls) != 1 || res.Calls[0].Name != "bash" || res.Calls[0].Arguments["command"] != "pwd" {
		t.Fatalf("%+v", res)
	}
}

func TestNestedQuoteCorruptionDropsInvoke(t *testing.T) {
	// Exact failure from live log (chatcmpl-059df584): model emitted
	// `<parameter name="parameter name="code" ...>`. Naive parsing yields
	// the bogus key `parameter name=`; must not be forwarded.
	raw := wrapCalls(dsmlOpen("invoke", ` name="run_code"`) +
		dsmlOpen("parameter", ` name="description" string="true"`) + "Read settings classes" + dsmlClose("parameter") +
		`<｜DSML｜parameter name="parameter name="code" string="true">` + "x" + dsmlClose("parameter") +
		dsmlClose("invoke"))
	res := ParseDSML(raw)
	if len(res.Calls) != 0 {
		t.Fatalf("corrupt invoke must be dropped, got %+v", res.Calls)
	}
	for _, c := range res.Calls {
		for k := range c.Arguments {
			if !isValidDSMLName(k) {
				t.Fatalf("invalid arg key forwarded: %q", k)
			}
		}
	}
}

func TestBogusAttrKeyNeverForwarded(t *testing.T) {
	attrs := parseAttrs(` name="parameter name="code" string="true"`)
	for k := range attrs {
		if !isValidDSMLName(k) {
			t.Fatalf("parseAttrs leaked invalid key %q: %v", k, attrs)
		}
	}
	if _, ok := attrs["parameter name="]; ok {
		t.Fatalf("bogus key must be dropped: %v", attrs)
	}
}

func TestEmptyArgsNeverEmitted(t *testing.T) {
	// An invoke that attempted params but yielded none must not become {}.
	raw := wrapCalls(dsmlOpen("invoke", ` name="read"`) +
		dsmlOpen("parameter", ``) + "x" + dsmlClose("parameter") +
		dsmlClose("invoke"))
	if res := ParseDSML(raw); len(res.Calls) != 0 {
		t.Fatalf("empty-args call must not be emitted: %+v", res.Calls)
	}
	// A genuinely parameter-less invoke (no <parameter> at all) still parses:
	// some tools legitimately take no args.
	rawNoParams := wrapCalls(dsmlOpen("invoke", ` name="ping"`) + dsmlClose("invoke"))
	if res := ParseDSML(rawNoParams); len(res.Calls) != 1 {
		t.Fatalf("no-param invoke must still parse: %+v", res)
	}
}
