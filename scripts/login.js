'use strict';
/**
 * Standalone headless-browser login for chat.deepseek.com.
 * Uses the installed Playwright Chromium (no downloads).
 * Writes state/session.json: { token, device_id, cookies, did, at }
 *
 * Usage: node scripts/login.js
 */
const fs = require('fs');
const path = require('path');
const { chromium } = require('playwright-core');

const ROOT = path.join(__dirname, '..');
const UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36';

// Auto-detect installed Chromium: CHROME_PATH > Playwright cache > system chrome.
function findChrome() {
  if (process.env.CHROME_PATH && fs.existsSync(process.env.CHROME_PATH)) return process.env.CHROME_PATH;
  const cache = path.join(process.env.HOME || process.env.USERPROFILE || '', '.cache', 'ms-playwright');
  try {
    for (const d of fs.readdirSync(cache)) {
      if (!d.startsWith('chromium-')) continue;
      for (const sub of fs.readdirSync(path.join(cache, d))) {
        for (const bin of ['chrome', 'chrome.exe', 'headless_shell', 'chrome-headless-shell']) {
          const p = path.join(cache, d, sub, bin);
          if (fs.existsSync(p)) return p;
        }
      }
    }
  } catch (_) {}
  for (const bin of ['chromium', 'chromium-browser', 'google-chrome', 'chrome']) {
    try {
      const out = require('child_process').execSync(
        (process.platform === 'win32' ? 'where ' : 'command -v ') + bin,
        { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim().split(/\r?\n/)[0];
      if (out && fs.existsSync(out)) return out;
    } catch (_) {}
  }
  return null;
}
const EXEC = findChrome();
if (!EXEC) {
  console.error('No Chromium found. Run setup first (npm install + browser download) or set CHROME_PATH.');
  process.exit(1);
}

async function main() {
  const creds = JSON.parse(fs.readFileSync(path.join(ROOT, 'account.json'), 'utf8'));
  let session = {};
  try { session = JSON.parse(fs.readFileSync(path.join(ROOT, 'state', 'session.json'), 'utf8')); } catch (_) {}
  if (!session.device_id) {
    session.device_id = 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
      const r = (Math.random() * 16) | 0;
      return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
    });
  }

  const browser = await chromium.launch({
    executablePath: EXEC, headless: true,
    args: ['--no-sandbox', '--disable-blink-features=AutomationControlled', '--disable-dev-shm-usage'],
  });
  const storageFile = path.join(ROOT, 'state', 'storage.json');
  const ctx = await browser.newContext({
    userAgent: UA, locale: 'en-US',
    ...(fs.existsSync(storageFile) ? { storageState: storageFile } : {}),
  });
  const page = await ctx.newPage();
  try {
    for (let i = 0; i < 5; i++) {
      try { await page.goto('https://chat.deepseek.com/sign_in', { waitUntil: 'domcontentloaded', timeout: 45000 }); break; }
      catch (e) { console.error('goto retry', i); await page.waitForTimeout(3000); }
    }
    // wait for device-fingerprint cookies (portal101) before submitting login
    for (let i = 0; i < 25; i++) {
      const cs = await ctx.cookies();
      if (cs.some(c => c.name.includes('thumbcache'))) break;
      await page.waitForTimeout(1000);
    }
    await page.waitForTimeout(4000);

    if (page.url() === 'https://chat.deepseek.com/') {
      const tok = await page.evaluate(() => {
        try { return JSON.parse(localStorage.getItem('userToken') || '{}').value || ''; } catch (_) { return ''; }
      });
      if (tok) { await finish(ctx, session, tok); await browser.close(); return; }
    }
    await page.waitForSelector('input[placeholder="Phone number / email address"]', { timeout: 30000 });
    await page.fill('input[placeholder="Phone number / email address"]', creds.account);
    await page.fill('input[placeholder="Password"]', creds.password);
    await page.waitForTimeout(1000);
    await page.locator('div.ds-button--primary', { hasText: 'Log in' }).first().click();
    console.error('login submitted, waiting for token...');
    let token = '';
    for (let i = 0; i < 40; i++) {
      await page.waitForTimeout(2000);
      if (page.url() === 'https://chat.deepseek.com/') {
        token = await page.evaluate(() => {
          try { return JSON.parse(localStorage.getItem('userToken') || '{}').value || ''; } catch (_) { return ''; }
        });
        if (token && token.length > 20) break;
      }
    }
    if (!token) throw new Error('Login failed: no token (possible captcha/rate-limit)');
    await finish(ctx, session, token);
  } finally {
    await browser.close();
  }
}

async function finish(ctx, session, token) {
  session.token = token;
  session.at = Date.now();
  const cookies = await ctx.cookies();
  session.cookies = cookies.map(c => ({ name: c.name, value: c.value })).filter(c => c.value);
  const ROOT2 = path.join(__dirname, '..');
  await ctx.storageState({ path: path.join(ROOT2, 'state', 'storage.json') });
  fs.writeFileSync(path.join(ROOT2, 'state', 'session.json'), JSON.stringify(session, null, 2));
  console.log(JSON.stringify({ ok: true, token_len: token.length, cookies: session.cookies.map(c => c.name) }));
}

main().catch(e => { console.error('login failed:', e.message); process.exit(1); });
