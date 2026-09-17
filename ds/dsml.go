package ds

// DSML adapter: translates DeepSeek web DSML tool calls into OpenAI-style
// tool calls.
//
// Observed wire formats (fullwidth vertical line U+FF5C "｜", single and
// doubled bars have both been seen on the wire):
//
//	<｜DSML｜calls> ... </｜DSML｜calls>
//	<｜｜DSML｜｜calls> ... </｜｜DSML｜｜calls>
//	<｜DSML｜invoke name="bash">
//	<｜DSML｜parameter name="command" string="true">
//	ls -la /home/duox/IdeaProjects
//	</｜DSML｜parameter>
//	</｜DSML｜invoke>
//
// The parser is a small stateful scanner (not regex-only): it walks the text,
// matches open/close tags, parses quoted attributes, and extracts invoke /
// parameter blocks. It tolerates whitespace/newline differences, one-or-more
// bars (fullwidth "｜" U+FF5C and ASCII "|", mixed), and truncated tags at
// streaming chunk boundaries. Malformed blocks fail safely: they are left as
// plain text and produce no tool calls.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCall is a normalized tool invocation extracted from DSML.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]string
}

// ParseResult is the outcome of parsing one complete assistant text.
type ParseResult struct {
	// CleanText is the original text with all complete DSML calls blocks removed.
	CleanText string
	Calls     []ToolCall
	// HasDSML reports whether at least one complete calls block was found.
	HasDSML bool
}

const (
	fwBar = "｜" // U+FF5C FULLWIDTH VERTICAL LINE (actual DeepSeek delimiter)
)

// dsmlTagNames are the tag keywords (without brackets).
var dsmlTagNames = []string{"calls", "invoke", "parameter"}

// fwBarBytes is the UTF-8 encoding of fwBar, used to detect a bar split
// mid-character at a streaming chunk boundary.
var fwBarBytes = []byte(fwBar)

// isFwBarPrefix reports whether tail (1-2 bytes) is a proper prefix of the
// UTF-8 encoding of the fullwidth bar (i.e. a bar split across chunks).
func isFwBarPrefix(tail string) bool {
	if len(tail) == 0 || len(tail) >= len(fwBarBytes) {
		return false
	}
	return strings.HasPrefix(string(fwBarBytes), tail)
}

// consumeBars consumes one or more consecutive bar characters (fullwidth "｜"
// U+FF5C and/or ASCII "|", mixed). DeepSeek has been observed emitting both
// single (<｜DSML｜calls>) and doubled (<｜｜DSML｜｜calls>) delimiters, so the
// parser accepts 1+. It returns the index just past the bars and true. If the
// string ends with a partial UTF-8 prefix of the fullwidth bar, it returns
// truncated=true so streaming hold-back treats it as a potential bar.
func consumeBars(s string, i int) (next int, ok bool, truncated bool) {
	count := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], fwBar) {
			i += len(fwBar)
			count++
			continue
		}
		if s[i] == '|' {
			i++
			count++
			continue
		}
		break
	}
	if count > 0 {
		return i, true, false
	}
	// No complete bar yet: check for a bar truncated at the string end
	// (either a partial UTF-8 sequence or end right after "</" / "<").
	// Let the caller decide; here report truncated if the tail looks like the
	// start of a fullwidth bar.
	if i < len(s) {
		return i, false, false
	}
	// i == len(s): examine trailing bytes for a partial bar.
	for k := 1; k < len(fwBarBytes); k++ {
		if len(s) >= k && isFwBarPrefix(s[len(s)-k:]) {
			// Only meaningful if what precedes is "<" or "</" plus bars.
			return i, false, true
		}
	}
	return i, false, false
}

// isDSMLPrefix reports whether suffix (text starting at some "<") could be the
// beginning of a DSML tag — either a full tag start or a truncation of one at
// a chunk boundary. Normal text like "a < b" returns false.
//
// Accepted shape (whitespace-tolerant):
//
//	'<' ['/'] bars+ 'DSML' bars+ [tag-prefix]
//	bar := '｜' (U+FF5C, 1+) | '|' (ASCII, 1+), mixed allowed
func isDSMLPrefix(suffix string) bool {
	if suffix == "" || suffix[0] != '<' {
		return false
	}
	i := 1
	if i < len(suffix) && suffix[i] == '/' {
		i++
	}
	// bars+ (or truncated)
	ni, ok, trunc := consumeBars(suffix, i)
	if trunc {
		return true
	}
	if !ok {
		// No bar yet: still a potential prefix only if we are at the very end
		// (e.g. suffix == "<" or "</") or the tail is a partial bar.
		if ni >= len(suffix) {
			return true // "<" or "</" — could grow into a tag
		}
		if isFwBarPrefix(suffix[ni:]) {
			return true
		}
		return false
	}
	i = ni
	// A complete bar followed by a partial bar (bar split across chunks):
	// more bar bytes may still arrive.
	if isFwBarPrefix(suffix[i:]) {
		return true
	}
	// 'DSML' (possibly truncated, e.g. "<｜D", "<｜DSM")
	if i >= len(suffix) {
		return true
	}
	if len(suffix[i:]) < 4 && strings.HasPrefix("DSML", suffix[i:]) {
		return true
	}
	if !strings.HasPrefix(suffix[i:], "DSML") {
		return false
	}
	i += 4
	ni, ok, trunc = consumeBars(suffix, i)
	if trunc {
		return true
	}
	if !ok {
		if ni >= len(suffix) {
			return true // e.g. "<｜DSML" — bars may follow
		}
		if isFwBarPrefix(suffix[ni:]) {
			return true
		}
		return false
	}
	i = ni
	// Complete bar(s) followed by a partial bar: hold for more bytes.
	if isFwBarPrefix(suffix[i:]) {
		return true
	}
	if i >= len(suffix) {
		return true
	}
	// optional whitespace before tag name
	for i < len(suffix) && (suffix[i] == ' ' || suffix[i] == '\t' || suffix[i] == '\n' || suffix[i] == '\r') {
		i++
	}
	if i >= len(suffix) {
		return true // whitespace at end — tag name may follow
	}
	rest := suffix[i:]
	if isFwBarPrefix(rest) {
		return true // partial bar where tag name or more bars may follow
	}
	for _, n := range dsmlTagNames {
		if strings.HasPrefix(rest, n) {
			return true
		}
		if strings.HasPrefix(n, rest) {
			return true // truncated tag name: "c", "cal", "invo", ...
		}
	}
	return false
}

// FirstDSMLStart returns the index of the first "<" that could start a DSML
// tag, or -1 if there is none. Used by the streaming filter to hold back a
// potential (possibly split) DSML tag until more bytes arrive.
func FirstDSMLStart(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		if isDSMLPrefix(s[i:]) {
			return i
		}
	}
	return -1
}

// parseAttrs parses name="value" pairs (double or single quoted) from a tag
// interior like ` name="bash" string="true" `. It tolerates extra whitespace.
func parseAttrs(s string) map[string]string {
	attrs := map[string]string{}
	i := 0
	for i < len(s) {
		// skip whitespace
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) || s[i] == '>' || s[i] == '/' {
			break
		}
		// attr name
		start := i
		for i < len(s) && s[i] != '=' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' && s[i] != '>' {
			i++
		}
		name := s[start:i]
		// skip whitespace before =
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			continue
		}
		i++ // =
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		q := s[i]
		if q != '"' && q != '\'' {
			// unquoted value: read until whitespace or >
			start = i
			for i < len(s) && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' && s[i] != '>' {
				i++
			}
			if name != "" {
				attrs[name] = s[start:i]
			}
			continue
		}
		i++
		start = i
		for i < len(s) && s[i] != q {
			i++
		}
		if name != "" {
			attrs[name] = s[start:i]
		}
		if i < len(s) {
			i++ // closing quote
		}
	}
	return attrs
}

// matchOpenTag tries to match a DSML open tag at s[pos:] (s[pos] must be '<').
// tag must be one of calls/invoke/parameter. On success it returns the attrs
// and the index just past '>'. It accepts one or more fullwidth and/or ASCII
// bars (single <｜DSML｜...> and doubled <｜｜DSML｜｜...> variants) and
// arbitrary whitespace between tokens, e.g. `<｜DSML｜invoke name="bash">`.
func matchOpenTag(s string, pos int, tag string) (map[string]string, int, bool) {
	if pos >= len(s) || s[pos] != '<' {
		return nil, 0, false
	}
	i := pos + 1
	// bars+: fullwidth (3 bytes in UTF-8) and/or ASCII, 1 or more
	ni, ok, _ := consumeBars(s, i)
	if !ok {
		return nil, 0, false
	}
	i = ni
	if !strings.HasPrefix(s[i:], "DSML") {
		return nil, 0, false
	}
	i += 4
	ni, ok, _ = consumeBars(s, i)
	if !ok {
		return nil, 0, false
	}
	i = ni
	// skip whitespace before tag name
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	if !strings.HasPrefix(s[i:], tag) {
		return nil, 0, false
	}
	i += len(tag)
	// attrs run until '>'
	end := strings.IndexByte(s[i:], '>')
	if end < 0 {
		return nil, 0, false // truncated -> not a complete tag
	}
	attrs := parseAttrs(s[i : i+end])
	return attrs, i + end + 1, true
}

// matchCloseTag matches `</｜DSML｜tag>` (single, doubled, or ASCII bar
// variants, whitespace tolerant).
func matchCloseTag(s string, pos int, tag string) (int, bool) {
	if pos >= len(s) || s[pos] != '<' {
		return 0, false
	}
	i := pos + 1
	if i >= len(s) || s[i] != '/' {
		return 0, false
	}
	i++
	ni, ok, _ := consumeBars(s, i)
	if !ok {
		return 0, false
	}
	i = ni
	if !strings.HasPrefix(s[i:], "DSML") {
		return 0, false
	}
	i += 4
	ni, ok, _ = consumeBars(s, i)
	if !ok {
		return 0, false
	}
	i = ni
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	if !strings.HasPrefix(s[i:], tag) {
		return 0, false
	}
	i += len(tag)
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	if i >= len(s) || s[i] != '>' {
		return 0, false
	}
	return i + 1, true
}

// findOpenTag scans for the next open tag of the given name at or after from.
func findOpenTag(s string, from int, tag string) (attrs map[string]string, openStart, openEnd int, ok bool) {
	for i := from; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		if a, e, m := matchOpenTag(s, i, tag); m {
			return a, i, e, true
		}
	}
	return nil, 0, 0, false
}

// trimParamValue removes surrounding CR/LF characters while preserving spaces
// and other significant whitespace (e.g. indented code). It also unescapes
// basic XML entities.
func trimParamValue(v string) string {
	v = strings.Trim(v, "\r\n")
	v = strings.ReplaceAll(v, "&lt;", "<")
	v = strings.ReplaceAll(v, "&gt;", ">")
	v = strings.ReplaceAll(v, "&quot;", `"`)
	v = strings.ReplaceAll(v, "&apos;", "'")
	v = strings.ReplaceAll(v, "&amp;", "&")
	return v
}

// parseCallsBlock parses the interior of one <calls>...</calls> block
// (blockStart = index just past the open tag, blockEnd = index of the close
// tag). It returns the tool calls found. Malformed invoke/parameter entries
// are skipped individually; the rest of the block still parses.
func parseCallsBlock(s string, blockStart, blockEnd int, idBase, idStart int) []ToolCall {
	var calls []ToolCall
	pos := blockStart
	n := 0
	for pos < blockEnd {
		attrs, istart, iend, ok := findOpenTag(s, pos, "invoke")
		if !ok || istart >= blockEnd {
			break
		}
		// find matching </invoke> — first close tag at depth 1 (no nesting
		// of invoke inside invoke is expected; a nested open is malformed).
		closePos := -1
		closeEnd := -1
		scan := iend
		for scan < blockEnd {
			if s[scan] != '<' {
				scan++
				continue
			}
			if _, _, m := matchOpenTag(s, scan, "invoke"); m {
				break // nested invoke: outer is malformed
			}
			if e, m := matchCloseTag(s, scan, "invoke"); m {
				closePos, closeEnd = scan, e
				break
			}
			scan++
		}
		if closePos < 0 {
			// Malformed: missing </invoke> — skip this invoke, keep scanning
			// after its open tag so a later well-formed invoke still parses.
			pos = iend
			continue
		}
		name := strings.TrimSpace(attrs["name"])
		if name == "" {
			pos = closeEnd
			continue // invoke without name: skip
		}
		args := map[string]string{}
		ppos := iend
		for ppos < closePos {
			pattrs, pstart, pend, pok := findOpenTag(s, ppos, "parameter")
			if !pok || pstart >= closePos {
				break
			}
			// find </parameter>
			pcpos := -1
			pcend := -1
			pscan := pend
			for pscan < closePos {
				if s[pscan] != '<' {
					pscan++
					continue
				}
				if _, _, m := matchOpenTag(s, pscan, "parameter"); m {
					break // nested parameter: malformed
				}
				if e, m := matchCloseTag(s, pscan, "parameter"); m {
					pcpos, pcend = pscan, e
					break
				}
				pscan++
			}
			if pcpos < 0 {
				ppos = pend // missing close: skip this parameter
				continue
			}
			pname := strings.TrimSpace(pattrs["name"])
			if pname == "" {
				ppos = pcend
				continue
			}
			args[pname] = trimParamValue(s[pend:pcpos])
			ppos = pcend
		}
		n++
		tc := ToolCall{
			ID:        fmt.Sprintf("call_dsml_%d", idStart+n),
			Name:      name,
			Arguments: args,
		}
		_ = idBase
		calls = append(calls, tc)
		pos = closeEnd
	}
	return calls
}

// ParseDSML extracts all complete DSML calls blocks from text.
// Incomplete/malformed blocks are left in the clean text (fail-safe: no
// hallucinated calls). CleanText has every complete calls block removed.
func ParseDSML(text string) ParseResult {
	var calls []ToolCall
	var out strings.Builder
	pos := 0
	found := false
	for {
		_, cstart, cend, ok := findOpenTag(text, pos, "calls")
		if !ok {
			out.WriteString(text[pos:])
			break
		}
		// find matching </calls>
		closePos := -1
		closeEnd := -1
		scan := cend
		for scan < len(text) {
			if text[scan] != '<' {
				scan++
				continue
			}
			if _, _, m := matchOpenTag(text, scan, "calls"); m {
				break // nested calls: outer malformed
			}
			if e, m := matchCloseTag(text, scan, "calls"); m {
				closePos, closeEnd = scan, e
				break
			}
			scan++
		}
		if closePos < 0 {
			// Missing close: fail safe — leave the rest as plain text.
			out.WriteString(text[pos:])
			break
		}
		found = true
		out.WriteString(text[pos:cstart])
		blockCalls := parseCallsBlock(text, cend, closePos, 0, len(calls))
		calls = append(calls, blockCalls...)
		pos = closeEnd
	}
	clean := strings.TrimSpace(out.String())
	// Collapse 3+ consecutive newlines left by block removal.
	for strings.Contains(clean, "\n\n\n") {
		clean = strings.ReplaceAll(clean, "\n\n\n", "\n\n")
	}
	return ParseResult{CleanText: clean, Calls: calls, HasDSML: found}
}

// ArgumentsJSON marshals a ToolCall's arguments to the JSON-object string
// expected by OpenAI function tool calls ({"k":"v",...}).
func (c ToolCall) ArgumentsJSON() string {
	if c.Arguments == nil {
		return "{}"
	}
	b, err := json.Marshal(c.Arguments)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// StreamFilter incrementally separates safe assistant text from (possibly
// chunk-split) DSML blocks. Feed each text delta via Write; it returns the
// newly-safe text prefix that can be forwarded immediately. At the end of the
// stream call Flush to get the remaining safe text plus all parsed calls.
//
// Guarantee: raw DSML bytes are never returned as safe text; safe text is
// always a prefix of the final CleanText.
type StreamFilter struct {
	raw      strings.Builder
	emitted  int // bytes of raw already released as safe text
	cleanLen int // bytes of CleanText already emitted
}

// Raw returns the full accumulated raw text (including buffered DSML).
// It is intended for debug logging (DSML_DEBUG) — never log it by default.
func (f *StreamFilter) Raw() string {
	return f.raw.String()
}

// Write accumulates a delta and returns newly safe text (may be "").
func (f *StreamFilter) Write(delta string) string {
	f.raw.WriteString(delta)
	raw := f.raw.String()
	start := FirstDSMLStart(raw[f.emitted:])
	var safeEnd int
	if start < 0 {
		safeEnd = len(raw)
	} else {
		safeEnd = f.emitted + start
	}
	if safeEnd <= f.emitted {
		return ""
	}
	out := raw[f.emitted:safeEnd]
	f.emitted = safeEnd
	return out
}

// Flush parses the full buffered raw text and returns the remaining safe text
// (suffix of CleanText after what Write already released) and all tool calls.
func (f *StreamFilter) Flush() (string, []ToolCall) {
	res := ParseDSML(f.raw.String())
	rest := ""
	if f.cleanLen < len(res.CleanText) {
		// Write already emitted raw[:emitted], which is a prefix of CleanText
		// (everything before the first DSML marker is untouched by parsing).
		// Reconcile defensively: only slice when the prefix matches.
		if len(res.CleanText) >= f.emitted && res.CleanText[:f.emitted] == f.raw.String()[:f.emitted] {
			rest = res.CleanText[f.emitted:]
			f.cleanLen = len(res.CleanText)
		} else {
			// Fallback (should not happen): emit CleanText suffix beyond what
			// was already sent, capped at non-negative length.
			if len(res.CleanText) > f.cleanLen {
				rest = res.CleanText[f.cleanLen:]
				f.cleanLen = len(res.CleanText)
			}
		}
	}
	return rest, res.Calls
}
