'use strict';
/**
 * DSML adapter (Node mirror of ds/dsml.go).
 * Parses DeepSeek web DSML tool calls into OpenAI-style tool calls.
 */
const FW_BAR = '｜'; // U+FF5C

const TAG_NAMES = ['calls', 'invoke', 'parameter'];

function isBarChar(ch) {
  return ch === FW_BAR || ch === '|';
}

// Consume 1+ consecutive bars (fullwidth and/or ASCII, mixed). Returns index
// past the bars, or -1 if there is no complete bar.
function consumeBars(s, i) {
  let count = 0;
  while (i < s.length && isBarChar(s[i])) { i++; count++; }
  return count > 0 ? i : -1;
}

// Accepted shape (whitespace-tolerant):
//   '<' ['/'] bars+ 'DSML' bars+ [tag-prefix], bars := (｜ || |)+
function isDSMLPrefix(suffix) {
  if (!suffix || suffix[0] !== '<') return false;
  let i = 1;
  if (suffix[i] === '/') i++;
  let ni = consumeBars(suffix, i);
  if (ni < 0) {
    // No bar yet: "<" or "</" alone could still grow into a tag.
    return i >= suffix.length;
  }
  i = ni;
  if (i >= suffix.length) return true;
  const restDSML = suffix.slice(i);
  if (restDSML.length < 4 && 'DSML'.startsWith(restDSML)) return true;
  if (!suffix.startsWith('DSML', i)) return false;
  i += 4;
  ni = consumeBars(suffix, i);
  if (ni < 0) return i >= suffix.length;
  i = ni;
  if (i >= suffix.length) return true;
  while (i < suffix.length && ' \t\n\r'.includes(suffix[i])) i++;
  if (i >= suffix.length) return true;
  const rest = suffix.slice(i);
  for (const n of TAG_NAMES) {
    if (rest.startsWith(n)) return true;
    if (n.startsWith(rest)) return true;
  }
  return false;
}

function firstDSMLStart(s) {
  for (let i = 0; i < s.length; i++) {
    if (s[i] !== '<') continue;
    if (isDSMLPrefix(s.slice(i))) return i;
  }
  return -1;
}

function parseAttrs(s) {
  const attrs = {};
  let i = 0;
  while (i < s.length) {
    while (i < s.length && ' \t\n\r'.includes(s[i])) i++;
    if (i >= s.length || s[i] === '>' || s[i] === '/') break;
    const start = i;
    while (i < s.length && !['=', ' ', '\t', '\n', '\r', '>'].includes(s[i])) i++;
    const name = s.slice(start, i);
    while (i < s.length && (s[i] === ' ' || s[i] === '\t')) i++;
    if (s[i] !== '=') continue;
    i++;
    while (i < s.length && (s[i] === ' ' || s[i] === '\t')) i++;
    if (i >= s.length) break;
    const q = s[i];
    if (q !== '"' && q !== "'") {
      const vs = i;
      while (i < s.length && !' \t\n\r>'.includes(s[i])) i++;
      if (name) attrs[name] = s.slice(vs, i);
      continue;
    }
    i++;
    const vs = i;
    while (i < s.length && s[i] !== q) i++;
    if (name) attrs[name] = s.slice(vs, i);
    if (i < s.length) i++;
  }
  return attrs;
}

function matchOpenTag(s, pos, tag) {
  if (s[pos] !== '<') return null;
  let i = consumeBars(s, pos + 1);
  if (i < 0) return null;
  if (!s.startsWith('DSML', i)) return null;
  i += 4;
  i = consumeBars(s, i);
  if (i < 0) return null;
  while (i < s.length && ' \t\n\r'.includes(s[i])) i++;
  if (!s.startsWith(tag, i)) return null;
  i += tag.length;
  const end = s.indexOf('>', i);
  if (end < 0) return null;
  return { attrs: parseAttrs(s.slice(i, end)), end: end + 1 };
}

function matchCloseTag(s, pos, tag) {
  if (s[pos] !== '<' || s[pos + 1] !== '/') return -1;
  let i = consumeBars(s, pos + 2);
  if (i < 0) return -1;
  if (!s.startsWith('DSML', i)) return -1;
  i += 4;
  i = consumeBars(s, i);
  if (i < 0) return -1;
  while (i < s.length && ' \t\n\r'.includes(s[i])) i++;
  if (!s.startsWith(tag, i)) return -1;
  i += tag.length;
  while (i < s.length && ' \t\n\r'.includes(s[i])) i++;
  if (s[i] !== '>') return -1;
  return i + 1;
}

function findOpenTag(s, from, tag) {
  for (let i = from; i < s.length; i++) {
    if (s[i] !== '<') continue;
    const m = matchOpenTag(s, i, tag);
    if (m) return { attrs: m.attrs, start: i, end: m.end };
  }
  return null;
}

function trimParamValue(v) {
  v = v.replace(/^[\r\n]+|[\r\n]+$/g, '');
  return v.split('&lt;').join('<').split('&gt;').join('>')
    .split('&quot;').join('"').split('&apos;').join("'").split('&amp;').join('&');
}

function parseCallsBlock(s, blockStart, blockEnd, idStart) {
  const calls = [];
  let pos = blockStart;
  let n = 0;
  while (pos < blockEnd) {
    const inv = findOpenTag(s, pos, 'invoke');
    if (!inv || inv.start >= blockEnd) break;
    // find matching close
    let closePos = -1, closeEnd = -1, scan = inv.end;
    while (scan < blockEnd) {
      if (s[scan] !== '<') { scan++; continue; }
      if (matchOpenTag(s, scan, 'invoke')) break;
      const e = matchCloseTag(s, scan, 'invoke');
      if (e > 0) { closePos = scan; closeEnd = e; break; }
      scan++;
    }
    if (closePos < 0) { pos = inv.end; continue; }
    const name = (inv.attrs.name || '').trim();
    if (!name) { pos = closeEnd; continue; }
    const args = {};
    let ppos = inv.end;
    while (ppos < closePos) {
      const pm = findOpenTag(s, ppos, 'parameter');
      if (!pm || pm.start >= closePos) break;
      let pcpos = -1, pcend = -1, pscan = pm.end;
      while (pscan < closePos) {
        if (s[pscan] !== '<') { pscan++; continue; }
        if (matchOpenTag(s, pscan, 'parameter')) break;
        const e = matchCloseTag(s, pscan, 'parameter');
        if (e > 0) { pcpos = pscan; pcend = e; break; }
        pscan++;
      }
      if (pcpos < 0) { ppos = pm.end; continue; }
      const pname = (pm.attrs.name || '').trim();
      if (!pname) { ppos = pcend; continue; }
      args[pname] = trimParamValue(s.slice(pm.end, pcpos));
      ppos = pcend;
    }
    n++;
    calls.push({ id: `call_dsml_${idStart + n}`, name, arguments: args });
    pos = closeEnd;
  }
  return calls;
}

function parseDSML(text) {
  const calls = [];
  let out = '';
  let pos = 0;
  let found = false;
  for (;;) {
    const c = findOpenTag(text, pos, 'calls');
    if (!c) { out += text.slice(pos); break; }
    let closePos = -1, closeEnd = -1, scan = c.end;
    while (scan < text.length) {
      if (text[scan] !== '<') { scan++; continue; }
      if (matchOpenTag(text, scan, 'calls')) break;
      const e = matchCloseTag(text, scan, 'calls');
      if (e > 0) { closePos = scan; closeEnd = e; break; }
      scan++;
    }
    if (closePos < 0) { out += text.slice(pos); break; }
    found = true;
    out += text.slice(pos, c.start);
    calls.push(...parseCallsBlock(text, c.end, closePos, calls.length));
    pos = closeEnd;
  }
  let clean = out.trim();
  while (clean.includes('\n\n\n')) clean = clean.split('\n\n\n').join('\n\n');
  return { cleanText: clean, calls, hasDSML: found };
}

function argsJSON(call) {
  return JSON.stringify(call.arguments || {});
}

class StreamFilter {
  constructor() { this.raw = ''; this.emitted = 0; }
  // Full accumulated raw text (incl. buffered DSML). For debug logging
  // (DSML_DEBUG) only — never log by default.
  getRaw() { return this.raw; }
  write(delta) {
    this.raw += delta;
    const rel = firstDSMLStart(this.raw.slice(this.emitted));
    const safeEnd = rel < 0 ? this.raw.length : this.emitted + rel;
    if (safeEnd <= this.emitted) return '';
    const out = this.raw.slice(this.emitted, safeEnd);
    this.emitted = safeEnd;
    return out;
  }
  flush() {
    const res = parseDSML(this.raw);
    let rest = '';
    if (res.cleanText.length >= this.emitted &&
        res.cleanText.slice(0, this.emitted) === this.raw.slice(0, this.emitted)) {
      rest = res.cleanText.slice(this.emitted);
    } else if (res.cleanText.length > 0) {
      rest = res.cleanText.slice(Math.min(this.emitted, res.cleanText.length));
    }
    return { rest, calls: res.calls };
  }
}

module.exports = { parseDSML, argsJSON, StreamFilter, firstDSMLStart };
