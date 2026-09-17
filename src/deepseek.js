'use strict';
/**
 * DeepSeek web client.
 *
 * Why a headless browser? chat.deepseek.com sits behind AWS WAF
 * (`x-amzn-waf-action: challenge` for plain HTTP clients), so all API
 * traffic must originate from a real browser context that holds valid
 * WAF cookies. We keep one persistent headless Chromium (the installed
 * Playwright chromium) for login + cookies, issue API calls with Node
 * fetch reusing the context cookies (streaming-friendly), and solve the
 * per-request PoW (DeepSeekHashV1) locally via the vendored WASM.
 */
const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright-core');
const pow = require('./pow');

const BASE = 'https://chat.deepseek.com';
const UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36';
const CLIENT_VERSION = '2.5.0';

function log(...a) { console.log('[deepseek]', ...a); }

class DeepSeekClient {
  constructor(opts = {}) {
    this.executablePath = opts.executablePath || process.env.CHROME_PATH ||
      (process.env.HOME + '/.cache/ms-playwright/chromium-1243/chrome-linux64/chrome');
    this.stateDir = opts.stateDir || path.join(__dirname, '..', 'state');
    this.creds = opts.creds || JSON.parse(fs.readFileSync(
      opts.credsFile || path.join(__dirname, '..', 'account.json'), 'utf8'));
    this.storageFile = path.join(this.stateDir, 'storage.json');
    this.metaFile = path.join(this.stateDir, 'meta.json');
    this.meta = { deviceId: null, token: null };
    this.browser = null;
    this.ctx = null;
    this.page = null;
    this._loginInFlight = null;
  }

  loadMeta() {
    try { Object.assign(this.meta, JSON.parse(fs.readFileSync(this.metaFile, 'utf8'))); } catch (_) {}
    if (!this.meta.deviceId) {
      this.meta.deviceId = 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
        const r = (Math.random() * 16) | 0;
        return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
      });
      this.saveMeta();
    }
  }

  saveMeta() {
    fs.mkdirSync(this.stateDir, { recursive: true });
    fs.writeFileSync(this.metaFile, JSON.stringify(this.meta, null, 2));
  }

  async start() {
    this.loadMeta();
    await pow.init();
    fs.mkdirSync(this.stateDir, { recursive: true });
    this.browser = await chromium.launch({
      executablePath: this.executablePath,
      headless: true,
      args: ['--no-sandbox', '--disable-blink-features=AutomationControlled', '--disable-dev-shm-usage'],
    });
    const storageState = fs.existsSync(this.storageFile) ? this.storageFile : undefined;
    this.ctx = await this.browser.newContext({
      userAgent: UA, locale: 'en-US',
      ...(storageState ? { storageState } : {}),
    });
    this.page = await this.ctx.newPage();
    await this.ensureLogin();
  }

  async stop() {
    try { await this.ctx?.close(); } catch (_) {}
    try { await this.browser?.close(); } catch (_) {}
  }

  baseHeaders(extra = {}) {
    const tzOffsetSec = -new Date().getTimezoneOffset() * 60;
    return {
      'User-Agent': UA,
      'Accept': '*/*',
      'Accept-Language': 'en-US',
      'Content-Type': 'application/json',
      'Origin': BASE,
      'Referer': BASE + '/',
      'x-client-version': CLIENT_VERSION,
      'x-client-platform': 'web',
      'x-client-locale': 'en_US',
      'x-client-bundle-id': 'com.deepseek.chat',
      'x-device-id': this.meta.deviceId,
      'x-device-model': '',
      'x-client-timezone-offset': String(tzOffsetSec),
      ...(this.meta.token ? { 'authorization': 'Bearer ' + this.meta.token } : {}),
      ...extra,
    };
  }

  async cookieHeader() {
    const cookies = await this.ctx.cookies();
    return cookies.map(c => `${c.name}=${c.value}`).join('; ');
  }

  /** Low-level API call through Node fetch with browser cookies (streaming-capable). */
  async apiFetch(path, { method = 'GET', body, headers = {}, referer } = {}) {
    const url = BASE + path;
    try {
      const res = await fetch(url, {
        method,
        headers: this.baseHeaders({
          'Cookie': await this.cookieHeader(),
          ...(referer ? { 'Referer': referer } : {}),
          ...headers,
        }),
        body: body !== undefined ? JSON.stringify(body) : undefined,
      });
      return res;
    } catch (e) {
      throw new Error(`fetch ${method} ${path} failed: ${e.cause?.message || e.cause?.code || e.message}`);
    }
  }

  async apiJson(path, opts = {}) {
    const res = await this.apiFetch(path, opts);
    const text = await res.text();
    let json = null;
    try { json = JSON.parse(text); } catch (_) {}
    return { status: res.status, json, text };
  }

  /** Verify current token/cookies; login via UI if needed. */
  async ensureLogin(force = false) {
    if (this._loginInFlight) return this._loginInFlight;
    this._loginInFlight = (async () => {
      if (!force && this.meta.token) {
        const { status, json } = await this.apiJson('/api/v0/chat_session/fetch_page?lte_cursor.pinned=false');
        if (status === 200 && json && json.code === 0) {
          log('existing session token OK');
          return;
        }
        log('existing token invalid, re-login...');
      }
      await this.loginViaUI();
    })();
    try { await this._loginInFlight; } finally { this._loginInFlight = null; }
  }

  async loginViaUI() {
    const page = this.page;
    for (let i = 0; i < 5; i++) {
      try {
        await page.goto(BASE + '/sign_in', { waitUntil: 'domcontentloaded', timeout: 45000 });
        break;
      } catch (e) { log('goto retry', i, e.message.split('\n')[0]); await page.waitForTimeout(3000); }
    }
    await page.waitForTimeout(5000);
    // Already logged in? (redirected to /)
    if (page.url() === BASE + '/') {
      const tok = await page.evaluate(() => {
        try { return JSON.parse(localStorage.getItem('userToken') || '{}').value || ''; } catch (_) { return ''; }
      });
      if (tok) { this.meta.token = tok; this.saveMeta(); await this.saveStorage(); return; }
    }
    await page.waitForSelector('input[placeholder="Phone number / email address"]', { timeout: 30000 });
    await page.fill('input[placeholder="Phone number / email address"]', this.creds.account);
    await page.fill('input[placeholder="Password"]', this.creds.password);
    await page.locator('div.ds-button--primary', { hasText: 'Log in' }).first().click();
    log('login submitted, waiting for token...');
    let token = '';
    for (let i = 0; i < 40; i++) {
      await page.waitForTimeout(2000);
      if (page.url() === BASE + '/') {
        token = await page.evaluate(() => {
          try { return JSON.parse(localStorage.getItem('userToken') || '{}').value || ''; } catch (_) { return ''; }
        });
        if (token && token.length > 20) break;
      }
    }
    if (!token) throw new Error('Login failed: no token after 80s (possible captcha/rate-limit)');
    this.meta.token = token;
    this.saveMeta();
    await this.saveStorage();
    log('login OK, token saved');
  }

  async saveStorage() {
    await this.ctx.storageState({ path: this.storageFile });
  }

  // ---------- chat sessions ----------

  async createSession() {
    await this.ensureLogin();
    let { status, json } = await this.apiJson('/api/v0/chat_session/create', { method: 'POST', body: {} });
    if (status === 401 || (json && json.code === 401)) {
      await this.ensureLogin(true);
      ({ status, json } = await this.apiJson('/api/v0/chat_session/create', { method: 'POST', body: {} }));
    }
    const s = json?.data?.biz_data?.chat_session || json?.data?.biz_data;
    if (!s || !s.id) throw new Error('createSession failed: ' + JSON.stringify(json).slice(0, 500));
    await this.saveStorage().catch(() => {});
    return s;
  }

  async listSessions() {
    await this.ensureLogin();
    const { json } = await this.apiJson('/api/v0/chat_session/fetch_page?lte_cursor.pinned=false');
    return json?.data?.biz_data?.chat_sessions || [];
  }

  async deleteSession(id) {
    await this.ensureLogin();
    const { json } = await this.apiJson('/api/v0/chat_session/delete', { method: 'POST', body: { chat_session_id: id } });
    return json;
  }

  // ---------- proof of work ----------

  async solvePow(targetPath = '/api/v0/chat/completion') {
    const { status, json } = await this.apiJson('/api/v0/chat/create_pow_challenge', {
      method: 'POST', body: { target_path: targetPath },
    });
    const c = json?.data?.biz_data?.challenge;
    if (status !== 200 || !c) throw new Error('create_pow_challenge failed: ' + JSON.stringify(json).slice(0, 500));
    const ch = {
      algorithm: c.algorithm, challenge: c.challenge, salt: c.salt,
      difficulty: c.difficulty, signature: c.signature, expireAt: c.expire_at,
    };
    const t0 = Date.now();
    const answer = await pow.solve(ch);
    log(`PoW solved in ${Date.now() - t0}ms (difficulty=${ch.difficulty}, answer=${answer})`);
    return pow.buildHeader(ch, answer, targetPath);
  }

  // ---------- completion (SSE) ----------

  /**
   * Stream a chat completion. Async generator yielding:
   *   { type: 'text'|'thinking', delta } ... then { type: 'done', title }
   */
  async *completionStream({ sessionId, prompt, thinkingEnabled = false, searchEnabled = false, parentMessageId = null }) {
    await this.ensureLogin();
    const powHeader = await this.solvePow();
    const referer = `${BASE}/a/chat/s/${sessionId}`;
    const res = await this.apiFetch('/api/v0/chat/completion', {
      method: 'POST',
      referer,
      headers: { 'x-ds-pow-response': powHeader },
      body: {
        chat_session_id: sessionId,
        parent_message_id: parentMessageId,
        prompt,
        model_type: 'default',
        ref_file_ids: [],
        thinking_enabled: thinkingEnabled,
        search_enabled: searchEnabled,
        action: null,
        preempt: false,
      },
    });
    if (res.status === 401) {
      await this.ensureLogin(true);
      yield* this.completionStream({ sessionId, prompt, thinkingEnabled, searchEnabled, parentMessageId });
      return;
    }
    if (res.status !== 200 || !res.body) {
      const t = await res.text().catch(() => '');
      throw new Error(`completion HTTP ${res.status}: ${t.slice(0, 500)}`);
    }

    const fragments = []; // [{type, content}]
    const fragType = (i) => (fragments[i] ? fragments[i].type : 'RESPONSE');
    let buf = '';
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let event = '';

    const applyData = function* (dataStr) {
      let d;
      try { d = JSON.parse(dataStr); } catch (_) { return; }
      // Full snapshot: {"v": {"response": {"fragments": [...]}}} or {"v": {"response": {"content": "..."}}}
      const resp = d?.v?.response;
      if (resp) {
        if (Array.isArray(resp.fragments)) {
          fragments.length = 0;
          for (const f of resp.fragments) fragments.push({ type: f.type || 'RESPONSE', content: f.content || '' });
          const seed = fragments.map(f => f.content).join('');
          if (seed) yield { type: fragType(fragments.length - 1) === 'THINKING' ? 'thinking' : 'text', delta: seed };
          return;
        }
        if (typeof resp.content === 'string' && resp.content) {
          fragments.length = 0;
          fragments.push({ type: 'RESPONSE', content: resp.content });
          yield { type: 'text', delta: resp.content };
          return;
        }
        if (typeof resp.thinking_content === 'string' && resp.thinking_content) {
          yield { type: 'thinking', delta: resp.thinking_content };
          return;
        }
      }
      // Delta append: {"p":"response/fragments/<i>/content","o":"APPEND","v":"..."}
      if (typeof d.p === 'string' && d.o === 'APPEND' && typeof d.v === 'string') {
        const m = d.p.match(/fragments\/(-?\d+)\/content/);
        if (m) {
          let idx = parseInt(m[1], 10);
          if (idx < 0) idx = fragments.length - 1;
          const t = fragType(idx);
          yield { type: t === 'THINKING' ? 'thinking' : 'text', delta: d.v };
          if (fragments[idx]) fragments[idx].content += d.v;
        } else {
          yield { type: 'text', delta: d.v };
        }
        return;
      }
      if (d?.p === 'response/status' && d?.v === 'FINISHED') yield { type: 'status', finished: true };
    };

    while (true) {
      const { done, value } = await reader.read();
      if (value) buf += decoder.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf('\n\n')) !== -1) {
        const block = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        let dataPayload = null;
        for (const line of block.split('\n')) {
          if (line.startsWith('event:')) event = line.slice(6).trim();
          else if (line.startsWith('data:')) dataPayload = line.slice(5).trim();
        }
        if (event === 'close') { yield { type: 'done' }; return; }
        if (event === 'title' && dataPayload) {
          try { yield { type: 'title', title: JSON.parse(dataPayload).content }; } catch (_) {}
          continue;
        }
        if (dataPayload) yield* applyData(dataPayload);
      }
      if (done) { yield { type: 'done' }; return; }
    }
  }

  /** Non-streaming convenience wrapper. */
  async complete(opts) {
    let text = '', thinking = '', title = '';
    for await (const ev of this.completionStream(opts)) {
      if (ev.type === 'text') text += ev.delta;
      else if (ev.type === 'thinking') thinking += ev.delta;
      else if (ev.type === 'title') title = ev.title;
    }
    return { text, thinking, title };
  }
}

module.exports = { DeepSeekClient };
