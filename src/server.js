'use strict';
/**
 * chat2api — DeepSeek web-chat → OpenAI-compatible API gateway.
 *
 *  GET  /health
 *  GET  /v1/models
 *  POST /v1/chat/completions      (stream + non-stream)
 *  POST /v1/completions           (legacy)
 *  POST /v1/sessions              (new chat session)
 *  GET  /v1/sessions              (list chat sessions)
 *  DELETE /v1/sessions/:id        (delete chat session)
 *
 * Env:
 *   PORT=8080  GATEWAY_API_KEY=<optional>  CHROME_PATH=<optional>
 */
const express = require('express');
const cors = require('cors');
const { DeepSeekClient } = require('./deepseek');
const { parseDSML, StreamFilter } = require('./dsml');

const PORT = parseInt(process.env.PORT || '8080', 10);
const API_KEY = process.env.GATEWAY_API_KEY || '';

const MODELS = [
  { id: 'deepseek-chat', object: 'model', created: 1789616000, owned_by: 'deepseek', thinking: false },
  { id: 'deepseek-reasoner', object: 'model', created: 1789616000, owned_by: 'deepseek', thinking: true },
];

function resolveModel(name) {
  const m = MODELS.find(x => x.id === name);
  return m || MODELS[0];
}

function contentToString(content) {
  if (content == null) return '';
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content.filter(p => p && (p.type === 'text' || p.type === 'input_text'))
      .map(p => p.text).join('\n');
  }
  return String(content);
}

function formatIncomingToolCalls(m) {
  const tcs = m.tool_calls;
  if (!Array.isArray(tcs) || tcs.length === 0) return '';
  let s = '\nAssistant tool calls:';
  for (const tc of tcs) {
    const fn = tc.function || {};
    s += `\n- id=${tc.id || ''} name=${fn.name || tc.name || ''} arguments=${fn.arguments || ''}`;
  }
  return s;
}

/** Flatten OpenAI messages[] into one DeepSeek prompt (tool-aware). */
function messagesToPrompt(messages) {
  if (!Array.isArray(messages) || messages.length === 0) return '';
  if (messages.length === 1) {
    const m = messages[0];
    return contentToString(m.content) + formatIncomingToolCalls(m);
  }
  return messages.map(m => {
    const content = contentToString(m.content);
    if (m.role === 'assistant') return `Assistant: ${content}${formatIncomingToolCalls(m)}`;
    if (m.role === 'system') return `System: ${content}`;
    if (m.role === 'tool' || m.role === 'function') {
      const name = m.name || '';
      const callId = m.tool_call_id || m.id || '';
      if (name && callId) return `Tool result for ${name} (id ${callId}):\n${content}`;
      if (name) return `Tool result for ${name}:\n${content}`;
      return `Tool result:\n${content}`;
    }
    return `User: ${content}`;
  }).join('\n\n');
}

/** Translate OpenAI tools[] into DSML usage instructions for DeepSeek web. */
function appendToolsSection(prompt, tools) {
  if (!Array.isArray(tools) || tools.length === 0) return prompt;
  let s = prompt + '\n\nAvailable tools (invoke them with DSML blocks). Schemas (JSON):';
  for (const t of tools) {
    const fn = t.function || t;
    if (!fn.name) continue;
    s += `\n- ${fn.name}`;
    if (fn.description) s += `: ${fn.description}`;
    if (fn.parameters) {
      try { s += ` Parameters: ${JSON.stringify(fn.parameters)}`; } catch (_) {}
    }
  }
  s += '\nDSML format (use the exact fullwidth delimiters):';
  s += '\n<｜DSML｜calls>\n<｜DSML｜invoke name="TOOL_NAME">\n<｜DSML｜parameter name="PARAM_NAME" string="true">\nVALUE\n</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜calls>';
  s += '\nRules: keep parameter names/values exact; multiple invokes and parameters allowed; text outside DSML blocks is the assistant reply.';
  return s;
}

function dsmlToOpenAIToolCalls(calls) {
  return calls.map(c => ({
    id: c.id, type: 'function',
    function: { name: c.name, arguments: JSON.stringify(c.arguments || {}) },
  }));
}

// ---- session affinity ----
// Same idea as the Go gateway: OpenAI clients resend the full messages[]
// each turn. If the new messages[] extends a previously seen conversation
// (prefix match), reuse the same DeepSeek web session and send only the
// incremental tail. A /new (reset/divergent history) matches nothing, so a
// fresh web session is created. Explicit session_id always wins;
// "new"/"auto" forces a fresh session. Disable with SESSION_AFFINITY=0.
const AFFINITY_MAX = 32;
const affinityEntries = []; // { sessionId, messages, prompt, updated }
function sessionAffinityEnabled() {
  return ['', '1', 'true', 'yes', 'on'].includes(String(process.env.SESSION_AFFINITY ?? '').toLowerCase().trim());
}
function isAutoSessionId(id) {
  return ['', 'new', '_new', 'auto'].includes(String(id ?? '').toLowerCase().trim());
}
function normAffinityMessages(msgs) {
  return (msgs || []).map(m => ({
    role: m.role || '',
    content: contentToString(m.content),
    name: m.name || '',
    toolCallId: m.tool_call_id || m.id || '',
    toolCalls: Array.isArray(m.tool_calls) ? m.tool_calls.map(tc => {
      const fn = tc.function || {};
      return `${tc.id || ''}|${fn.name || tc.name || ''}|${fn.arguments || ''}`;
    }).join(';') : '',
  }));
}
function affinityPrefix(oldA, curA) {
  if (!oldA.length || oldA.length > curA.length) return false;
  for (let i = 0; i < oldA.length; i++) {
    const a = oldA[i], b = curA[i];
    if (a.role !== b.role || a.content !== b.content || a.name !== b.name ||
        a.toolCallId !== b.toolCallId || a.toolCalls !== b.toolCalls) return false;
  }
  return true;
}
function findAffinity(cur) {
  let best = null;
  for (const e of affinityEntries) {
    if (!e.messages || !e.messages.length) continue;
    if (!affinityPrefix(e.messages, cur)) continue;
    if (!best || e.messages.length > best.messages.length ||
        (e.messages.length === best.messages.length && e.updated > best.updated)) best = e;
  }
  return best;
}
function findAffinityByPrompt(prompt) {
  if (!prompt) return null;
  let best = null;
  for (const e of affinityEntries) {
    if (!e.prompt || (e.messages && e.messages.length)) continue;
    if (!prompt.startsWith(e.prompt)) continue;
    if (!best || e.prompt.length > best.prompt.length) best = e;
  }
  return best;
}
function upsertAffinity(sessionId, msgs, prompt) {
  if (!sessionId) return;
  const norm = normAffinityMessages(msgs);
  const now = Date.now();
  const ex = affinityEntries.find(e => e.sessionId === sessionId);
  if (ex) { ex.messages = norm; ex.prompt = prompt; ex.updated = now; return; }
  affinityEntries.push({ sessionId, messages: norm, prompt, updated: now });
  if (affinityEntries.length > AFFINITY_MAX) {
    affinityEntries.sort((a, b) => a.updated - b.updated);
    affinityEntries.splice(0, affinityEntries.length - AFFINITY_MAX);
  }
}

// DSML parser toggle. Default ON; set DSML_ENABLED=0|false|no|off to pass
// DeepSeek response text through verbatim (no tool_calls ever emitted).
function dsmlParsingEnabled() {
  return ['', '1', 'true', 'yes', 'on'].includes(String(process.env.DSML_ENABLED ?? '').toLowerCase().trim());
}

// Convert raw DeepSeek response text to OpenAI content/tool_calls/finish,
// honoring the DSML_ENABLED toggle.
function adaptDSML(text) {
  if (!dsmlParsingEnabled()) return { content: text, toolCalls: null, finish: 'stop' };
  const parsed = parseDSML(text);
  if (!parsed.calls.length) return { content: parsed.cleanText, toolCalls: null, finish: 'stop' };
  return { content: parsed.cleanText, toolCalls: dsmlToOpenAIToolCalls(parsed.calls), finish: 'tool_calls' };
}

// Opt-in raw logging via DSML_DEBUG=1|true|yes|raw|on (stderr, truncated).
// NEVER logs secrets: no API keys, tokens, cookies, or auth headers — only
// the DeepSeek prompt/response text and parsed tool calls.
function dsmlDebugEnabled() {
  return ['1', 'true', 'yes', 'raw', 'on'].includes(String(process.env.DSML_DEBUG || '').toLowerCase().trim());
}

// Code-point-safe truncation (never splits ｜ etc.).
function previewStr(s, n) {
  const a = Array.from(String(s ?? ''));
  return a.length <= n ? a.join('') : a.slice(0, n).join('') + `…[${a.length - n} more chars]`;
}

function logDSMLRaw(tag, raw, parsed) {
  if (!dsmlDebugEnabled()) return;
  console.error(`[dsml] ${tag}: raw_len=${raw.length} hasDSML=${parsed.hasDSML} calls=${parsed.calls.length} clean=${JSON.stringify(previewStr(parsed.cleanText, 500))}`);
  console.error(`[dsml] ${tag} raw preview: ${JSON.stringify(previewStr(raw, 4000))}`);
  parsed.calls.forEach((c, i) => console.error(
    `[dsml] ${tag} call[${i}]: name=${JSON.stringify(c.name)} args=${previewStr(JSON.stringify(c.arguments || {}), 1000)}`));
}

function checkAuth(req, res, next) {
  if (!API_KEY) return next();
  const h = req.headers.authorization || '';
  if (h === `Bearer ${API_KEY}`) return next();
  return res.status(401).json({ error: { message: 'Invalid API key', type: 'invalid_request_error' } });
}

async function main() {
  const ds = new DeepSeekClient();
  await ds.start();

  const app = express();
  app.use(cors());
  app.use(express.json({ limit: '5mb' }));
  app.use(checkAuth);

  app.get('/health', (req, res) => res.json({ status: 'ok' }));

  app.get('/v1/models', (req, res) => {
    res.json({ object: 'list', data: MODELS.map(({ thinking, ...m }) => m) });
  });

  // ---- sessions ----
  app.post('/v1/sessions', async (req, res) => {
    try {
      const s = await ds.createSession();
      res.json({ id: s.id, object: 'session', seq_id: s.seq_id, model_type: s.model_type, created: s.inserted_at });
    } catch (e) { res.status(502).json({ error: { message: String(e.message || e) } }); }
  });

  app.get('/v1/sessions', async (req, res) => {
    try {
      const list = await ds.listSessions();
      res.json({ object: 'list', data: list.map(s => ({ id: s.id, title: s.title, updated_at: s.updated_at })) });
    } catch (e) { res.status(502).json({ error: { message: String(e.message || e) } }); }
  });

  app.delete('/v1/sessions/:id', async (req, res) => {
    try {
      await ds.deleteSession(req.params.id);
      res.json({ id: req.params.id, object: 'session', deleted: true });
    } catch (e) { res.status(502).json({ error: { message: String(e.message || e) } }); }
  });

  // ---- chat completions ----
  app.post('/v1/chat/completions', async (req, res) => {
    try {
      const { model: modelName, messages, tools, stream, session_id, thinking_enabled, search_enabled } = req.body || {};
      const model = resolveModel(modelName);
      const fullPrompt = appendToolsSection(messagesToPrompt(messages), tools);
      if (!fullPrompt) return res.status(400).json({ error: { message: 'messages is required' } });

      const thinking = thinking_enabled ?? model.thinking;
      let prompt = fullPrompt;
      let sessionId = session_id;
      let reused = false;
      if (!isAutoSessionId(sessionId)) {
        // Explicit session_id wins.
      } else if (!sessionAffinityEnabled()) {
        sessionId = (await ds.createSession()).id;
      } else {
        const cur = normAffinityMessages(messages);
        const match = findAffinity(cur);
        if (match && cur.length >= match.messages.length) {
          sessionId = match.sessionId;
          reused = true;
          let tail = messages.slice(match.messages.length);
          if (!tail.length && messages.length) tail = messages.slice(-1);
          if (tail.length) {
            prompt = appendToolsSection(messagesToPrompt(tail), tools) || fullPrompt;
          }
        } else {
          sessionId = (await ds.createSession()).id;
        }
      }
      if (reused) console.log(`[affinity] reuse web session ${String(sessionId).slice(0, 8)} (msgs ${(messages || []).length})`);
      if (dsmlDebugEnabled()) {
        console.error(`[dsml] prompt chatcmpl-${String(sessionId).slice(0, 8)}: len=${prompt.length} tools=${Array.isArray(tools) ? tools.length : 0} preview=${JSON.stringify(previewStr(prompt, 1000))}`);
      }
      const gen = ds.completionStream({ sessionId, prompt, thinkingEnabled: !!thinking, searchEnabled: !!search_enabled });
      const created = Math.floor(Date.now() / 1000);
      const respModel = model.id;

      if (stream) {
        res.writeHead(200, {
          'Content-Type': 'text/event-stream',
          'Cache-Control': 'no-cache',
          Connection: 'keep-alive',
          'X-Accel-Buffering': 'no',
        });
        const chunk = (obj) => res.write(`data: ${JSON.stringify(obj)}\n\n`);
        const base = { id: `chatcmpl-${sessionId.slice(0, 8)}`, object: 'chat.completion.chunk', created, model: respModel, session_id: sessionId };
        let closed = false;
        req.on('close', () => { closed = true; });
        try {
          const filter = new StreamFilter();
          const dsmlPassthrough = !dsmlParsingEnabled();
          for await (const ev of gen) {
            if (closed) break;
            if (ev.type === 'text') {
              if (dsmlPassthrough) {
                chunk({ ...base, choices: [{ index: 0, delta: { content: ev.delta }, finish_reason: null }] });
                continue;
              }
              const safe = filter.write(ev.delta);
              if (safe) chunk({ ...base, choices: [{ index: 0, delta: { content: safe }, finish_reason: null }] });
            }
            else if (ev.type === 'thinking' && thinking) chunk({ ...base, choices: [{ index: 0, delta: { reasoning_content: ev.delta }, finish_reason: null }] });
            else if (ev.type === 'done') break;
          }
          if (!closed) {
            let { rest, calls } = filter.flush();
            if (dsmlPassthrough) {
              rest = ''; calls = [];
              if (dsmlDebugEnabled()) console.error(`[dsml] stream chatcmpl-${String(sessionId).slice(0, 8)}: parser disabled, passthrough`);
            } else if (dsmlDebugEnabled()) {
              logDSMLRaw(`stream chatcmpl-${String(sessionId).slice(0, 8)}`, filter.getRaw(), parseDSML(filter.getRaw()));
            }
            if (rest) chunk({ ...base, choices: [{ index: 0, delta: { content: rest }, finish_reason: null }] });
            calls.forEach((c, i) => chunk({
              ...base, choices: [{
                index: 0,
                delta: {
                  tool_calls: [{
                    index: i, id: c.id, type: 'function',
                    function: { name: c.name, arguments: JSON.stringify(c.arguments || {}) },
                  }],
                }, finish_reason: null,
              }],
            }));
            chunk({ ...base, choices: [{ index: 0, delta: {}, finish_reason: calls.length ? 'tool_calls' : 'stop' }] });
          }
        } catch (e) {
          chunk({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'error' }], error: String(e.message || e) });
        }
        res.write('data: [DONE]\n\n');
        res.end();
        upsertAffinity(sessionId, messages, fullPrompt);
      } else {
        let text = '', reasoning = '';
        for await (const ev of gen) {
          if (ev.type === 'text') text += ev.delta;
          else if (ev.type === 'thinking') reasoning += ev.delta;
          else if (ev.type === 'done') break;
        }
        const { content, toolCalls, finish } = adaptDSML(text);
        if (dsmlDebugEnabled()) {
          if (dsmlParsingEnabled()) {
            logDSMLRaw(`non-stream chatcmpl-${String(sessionId).slice(0, 8)}`, text, parseDSML(text));
          } else {
            console.error(`[dsml] non-stream chatcmpl-${String(sessionId).slice(0, 8)}: parser disabled, passthrough len=${text.length}`);
          }
        }
        const msg = { role: 'assistant', content };
        if (thinking && reasoning) msg.reasoning_content = reasoning;
        if (toolCalls) msg.tool_calls = toolCalls;
        upsertAffinity(sessionId, messages, fullPrompt);
        res.json({
          id: `chatcmpl-${sessionId.slice(0, 8)}`, object: 'chat.completion', created, model: respModel,
          session_id: sessionId,
          choices: [{ index: 0, message: msg, finish_reason: finish }],
          usage: { prompt_tokens: -1, completion_tokens: -1, total_tokens: -1 },
        });
      }
    } catch (e) {
      if (!res.headersSent) res.status(502).json({ error: { message: String(e.message || e) } });
      else res.end();
    }
  });

  // ---- legacy completions ----
  app.post('/v1/completions', async (req, res) => {
    try {
      const { model: modelName, prompt: rawPrompt, stream, session_id } = req.body || {};
      const model = resolveModel(modelName);
      const fullText = typeof rawPrompt === 'string' ? rawPrompt : messagesToPrompt([{ role: 'user', content: rawPrompt }]);
      let text = fullText;
      let sessionId = session_id;
      if (isAutoSessionId(sessionId)) {
        if (!sessionAffinityEnabled()) {
          sessionId = (await ds.createSession()).id;
        } else {
          const m = findAffinityByPrompt(fullText);
          if (m) {
            sessionId = m.sessionId;
            const tail = fullText.slice(m.prompt.length);
            if (tail.trim()) text = tail;
          } else {
            sessionId = (await ds.createSession()).id;
          }
        }
      } else if (!sessionId) {
        sessionId = (await ds.createSession()).id;
      }
      const created = Math.floor(Date.now() / 1000);
      if (stream) {
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive' });
        const base = { id: `cmpl-${sessionId.slice(0, 8)}`, object: 'text_completion', created, model: model.id };
        for await (const ev of ds.completionStream({ sessionId, prompt: text })) {
          if (ev.type === 'text') res.write(`data: ${JSON.stringify({ ...base, choices: [{ text: ev.delta, index: 0, finish_reason: null }] })}\n\n`);
          else if (ev.type === 'done') break;
        }
        res.write(`data: ${JSON.stringify({ ...base, choices: [{ text: '', index: 0, finish_reason: 'stop' }] })}\n\n`);
        res.write('data: [DONE]\n\n');
        res.end();
        upsertAffinity(sessionId, [], fullText);
      } else {
        const { text: out } = await ds.complete({ sessionId, prompt: text });
        upsertAffinity(sessionId, [], fullText);
        res.json({
          id: `cmpl-${sessionId.slice(0, 8)}`, object: 'text_completion', created, model: model.id,
          session_id: sessionId, choices: [{ text: out, index: 0, finish_reason: 'stop' }],
          usage: { prompt_tokens: -1, completion_tokens: -1, total_tokens: -1 },
        });
      }
    } catch (e) {
      if (!res.headersSent) res.status(502).json({ error: { message: String(e.message || e) } });
      else res.end();
    }
  });

  const server = app.listen(PORT, () => console.log(`chat2api listening on :${PORT}`));

  const shutdown = async () => {
    console.log('\nshutting down...');
    server.close();
    await ds.stop();
    process.exit(0);
  };
  process.on('SIGINT', shutdown);
  process.on('SIGTERM', shutdown);
}

main().catch(e => { console.error('fatal:', e); process.exit(1); });
