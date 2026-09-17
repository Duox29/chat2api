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

/** Flatten OpenAI messages[] into one DeepSeek prompt. */
function messagesToPrompt(messages) {
  if (!Array.isArray(messages) || messages.length === 0) return '';
  if (messages.length === 1) return String(messages[0].content ?? '');
  return messages.map(m => {
    const role = m.role === 'assistant' ? 'Assistant' : m.role === 'system' ? 'System' : 'User';
    const content = Array.isArray(m.content)
      ? m.content.filter(p => p.type === 'text').map(p => p.text).join('\n')
      : String(m.content ?? '');
    return `${role}: ${content}`;
  }).join('\n\n');
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
      const { model: modelName, messages, stream, session_id, thinking_enabled, search_enabled } = req.body || {};
      const model = resolveModel(modelName);
      const prompt = messagesToPrompt(messages);
      if (!prompt) return res.status(400).json({ error: { message: 'messages is required' } });

      const thinking = thinking_enabled ?? model.thinking;
      const sessionId = session_id || (await ds.createSession()).id;
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
        const base = { id: `chatcmpl-${sessionId.slice(0, 8)}`, object: 'chat.completion.chunk', created, model: respModel };
        let closed = false;
        req.on('close', () => { closed = true; });
        try {
          for await (const ev of gen) {
            if (closed) break;
            if (ev.type === 'text') chunk({ ...base, choices: [{ index: 0, delta: { content: ev.delta }, finish_reason: null }] });
            else if (ev.type === 'thinking' && thinking) chunk({ ...base, choices: [{ index: 0, delta: { reasoning_content: ev.delta }, finish_reason: null }] });
            else if (ev.type === 'done') break;
          }
        } catch (e) {
          chunk({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'error' }], error: String(e.message || e) });
        }
        chunk({ ...base, choices: [{ index: 0, delta: {}, finish_reason: 'stop' }] });
        res.write('data: [DONE]\n\n');
        res.end();
      } else {
        let text = '', reasoning = '';
        for await (const ev of gen) {
          if (ev.type === 'text') text += ev.delta;
          else if (ev.type === 'thinking') reasoning += ev.delta;
          else if (ev.type === 'done') break;
        }
        const msg = { role: 'assistant', content: text };
        if (thinking && reasoning) msg.reasoning_content = reasoning;
        res.json({
          id: `chatcmpl-${sessionId.slice(0, 8)}`, object: 'chat.completion', created, model: respModel,
          session_id: sessionId,
          choices: [{ index: 0, message: msg, finish_reason: 'stop' }],
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
      const { model: modelName, prompt, stream, session_id } = req.body || {};
      const model = resolveModel(modelName);
      const text = typeof prompt === 'string' ? prompt : messagesToPrompt([{ role: 'user', content: prompt }]);
      const sessionId = session_id || (await ds.createSession()).id;
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
      } else {
        const { text: out } = await ds.complete({ sessionId, prompt: text });
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
