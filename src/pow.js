'use strict';
/**
 * DeepSeekHashV1 proof-of-work solver.
 *
 * Exact port of DeepSeek's own pure-JS fallback worker (frontend chunk 76608):
 *
 *   prefix = `${salt}_${expireAt}_`
 *   answer = smallest i in [0, difficulty) with
 *            DeepSeekHash(prefix + String(i)).hexdigest() === challenge
 *
 * NOTE: DeepSeekHash is Keccak-256 with a *byte-swapped lane encoding*
 * (absorb/squeeze XOR words big-endian-swapped vs standard Keccak), so
 * Node's built-in SHA3-256 does NOT match — hence this verbatim port.
 *
 * Header sent as:
 *   x-ds-pow-response: base64({algorithm, challenge, salt, answer, signature, target_path})
 */

// ---- lane-copy helper: copy 64-bit lane (2x u32) src[e] -> dst[n] ----
function laneCopier() {
  return (t, e) => (r, n) => {
    const i = 2 * n, o = 2 * e;
    r[i] = t[o]; r[i + 1] = t[o + 1];
  };
}

// chi
function chi(st) {
  const cp = laneCopier();
  const { A: e, C: r } = st;
  for (let t = 0; t < 25; t += 5) {
    for (let n = 0; n < 5; n++) cp(e, t + n)(r, n);
    for (let n = 0; n < 5; n++) {
      const i = (t + n) * 2, o = ((n + 1) % 5) * 2, f = ((n + 2) % 5) * 2;
      e[i] ^= ~r[o] & r[f];
      e[i + 1] ^= ~r[o + 1] & r[f + 1];
    }
  }
}

// round constants (standard Keccak RC[i] as (lo,hi) pairs, index 2*i)
const RC = new Uint32Array([
  0, 1, 0, 32898, 0x80000000, 32906, 0x80000000, 0x80008000, 0, 32907,
  0, 0x80000001, 0x80000000, 0x80008081, 0x80000000, 32777, 0, 138, 0, 136,
  0, 0x80008009, 0, 0x8000000a, 0, 0x8000808b, 0x80000000, 139, 0x80000000, 32905,
  0x80000000, 32771, 0x80000000, 32770, 0x80000000, 128, 0, 32778,
  0x80000000, 0x8000000a, 0x80000000, 0x80008081, 0x80000000, 32896,
  0, 0x80000001, 0x80000000, 0x80008008,
]);

// iota
function iota(st) {
  const { A: e, I: r } = st, n = 2 * r;
  e[0] ^= RC[n]; e[1] ^= RC[n + 1];
}

const RHO_IDX = [10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1];
const RHO_OFF = [1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44];

// rho + pi
function rhoPi(st) {
  const cp = laneCopier();
  const { A: e, C: r, W: n } = st;
  let i = 0;
  cp(e, i + 1)(n, i);
  let o = 0, f = 0, s = 0, u = 32;
  for (; i < 24; i++) {
    const t = RHO_IDX[i], a = RHO_OFF[i];
    cp(e, t)(r, 0);
    o = n[0]; f = n[1]; u = 32 - a;
    n[s = a < 32 ? 0 : 1] = (o << a) | (f >>> u);
    n[(s + 1) % 2] = (f << a) | (o >>> u);
    cp(n, 0)(e, t);
    cp(r, 0)(n, 0);
  }
}

// theta
function theta(st) {
  const cp = laneCopier();
  const { A: e, C: r, D: n, W: i } = st;
  let o = 0, f = 0;
  for (let t = 0; t < 5; t++) {
    const n2 = 2 * t, i2 = (t + 5) * 2, o2 = (t + 10) * 2, f2 = (t + 15) * 2, s2 = (t + 20) * 2;
    r[n2] = e[n2] ^ e[i2] ^ e[o2] ^ e[f2] ^ e[s2];
    r[n2 + 1] = e[n2 + 1] ^ e[i2 + 1] ^ e[o2 + 1] ^ e[f2 + 1] ^ e[s2 + 1];
  }
  for (let t = 0; t < 5; t++) {
    cp(r, (t + 1) % 5)(i, 0);
    o = i[0]; f = i[1];
    i[0] = (o << 1) | (f >>> 31);
    i[1] = (f << 1) | (o >>> 31);
    n[2 * t] = r[((t + 4) % 5) * 2] ^ i[0];
    n[2 * t + 1] = r[((t + 4) % 5) * 2 + 1] ^ i[1];
    for (let r2 = 0; r2 < 25; r2 += 5) {
      e[(r2 + t) * 2] ^= n[2 * t];
      e[(r2 + t) * 2 + 1] ^= n[2 * t + 1];
    }
  }
}

// absorb a byte queue into state (byte-swapped lane encoding — verbatim)
function xorIn(t, e) {
  for (let r = 0; r < t.length; r += 8) {
    const n = r / 4;
    e[n] ^= (t[r + 7] << 24) | (t[r + 6] << 16) | (t[r + 5] << 8) | t[r + 4];
    e[n + 1] ^= (t[r + 3] << 24) | (t[r + 2] << 16) | (t[r + 1] << 8) | t[r];
  }
  return e;
}

// squeeze state out to bytes (byte-swapped lane encoding — verbatim)
function squeezeOut(t, e) {
  for (let r = 0; r < e.length; r += 8) {
    const n = r / 4;
    e[r] = t[n + 1];
    e[r + 1] = t[n + 1] >>> 8;
    e[r + 2] = t[n + 1] >>> 16;
    e[r + 3] = t[n + 1] >>> 24;
    e[r + 4] = t[n];
    e[r + 5] = t[n] >>> 8;
    e[r + 6] = t[n] >>> 16;
    e[r + 7] = t[n] >>> 24;
  }
  return e;
}

class Sponge {
  constructor(capacity = 256) {
    this.rate = 200 - capacity / 4;   // 136
    this.outLen = capacity / 8;       // 32
    this.keccakTmpC = new Uint32Array(10);
    this.keccakTmpD = new Uint32Array(10);
    this.keccakTmpW = new Uint32Array(2);
    this.state = new Uint32Array(50);
    this.queue = Buffer.allocUnsafe(this.rate);
    this.queueOffset = 0;
  }
  permute() {
    const s = this.state;
    for (let i = 1; i < 24; i++) {
      theta({ A: s, C: this.keccakTmpC, D: this.keccakTmpD, W: this.keccakTmpW });
      rhoPi({ A: s, C: this.keccakTmpC, W: this.keccakTmpW });
      chi({ A: s, C: this.keccakTmpC });
      iota({ A: s, I: i });
    }
    this.keccakTmpC.fill(0); this.keccakTmpD.fill(0); this.keccakTmpW.fill(0);
  }
  absorb(bytes) {
    for (let e = 0; e < bytes.length; e++) {
      this.queue[this.queueOffset] = bytes[e];
      this.queueOffset += 1;
      if (this.queueOffset >= this.rate) {
        xorIn(this.queue, this.state);
        this.permute();
        this.queueOffset = 0;
      }
    }
    return this;
  }
  update(str) {
    if (typeof str !== 'string') throw new TypeError('input not a string');
    return this.absorb(Buffer.from(str, 'utf8'));
  }
  copy() {
    const c = new Sponge();
    c.state.set(this.state);
    this.queue.copy(c.queue);
    c.queueOffset = this.queueOffset;
    return c;
  }
  digest(encoding = 'hex') {
    const out = Buffer.allocUnsafe(this.outLen);
    const queue = Buffer.allocUnsafe(this.queue.length);
    this.queue.copy(queue);
    const state = new Uint32Array(this.state);
    queue.fill(0, this.queueOffset);
    queue[this.queueOffset] |= 0x06;              // SHA3 domain separation
    queue[this.rate - 1] |= 0x80;
    xorIn(queue, state);
    for (let t = 0; t < out.length; t += this.rate) {
      // fresh temporaries (permute uses this.* — replicate with locals)
      const C = new Uint32Array(10), D = new Uint32Array(10), W = new Uint32Array(2);
      for (let i = 1; i < 24; i++) {
        theta({ A: state, C, D, W });
        rhoPi({ A: state, C, W });
        chi({ A: state, C });
        iota({ A: state, I: i });
      }
      squeezeOut(state, out.slice(t, t + this.rate));
    }
    return out.toString(encoding);
  }
  reset() {
    this.queue.fill(0); this.state.fill(0); this.queueOffset = 0;
    return this;
  }
}

function init() { return Promise.resolve(true); }

/**
 * Solve a PoW challenge.
 * @param {object} ch {algorithm, challenge, salt, difficulty, signature, expireAt}
 * @returns {Promise<number>} answer nonce
 */
async function solve(ch) {
  if (ch.algorithm !== 'DeepSeekHashV1') throw new Error('Unsupported PoW algorithm: ' + ch.algorithm);
  if (typeof ch.challenge !== 'string' || ch.challenge.length % 2 !== 0) throw new Error('Bad PoW challenge');
  if (!Number.isSafeInteger(ch.difficulty) || ch.difficulty <= 0) throw new Error('Bad PoW difficulty');
  const prefix = `${ch.salt}_${ch.expireAt}_`;
  const base = new Sponge(256).update(prefix);
  for (let i = 0; i < ch.difficulty; i++) {
    if (base.copy().update(String(i)).digest('hex') === ch.challenge) return i;
    if ((i & 1023) === 1023) await new Promise(r => setImmediate(r)); // don't block the loop
  }
  throw new Error(`PoW solve failed: no solution in [0, ${ch.difficulty})`);
}

/** Self-test against a ground-truth vector solved by DeepSeek's own worker. */
async function selfTest() {
  const v = require('/tmp/opencode/pow-vector.json');
  const prefix = `${v.challenge.salt}_${v.challenge.expire_at}_`;
  const h = new Sponge(256).update(prefix + String(v.answer)).digest('hex');
  return { match: h === v.challenge.challenge, computed: h, expected: v.challenge.challenge };
}

/** Build the x-ds-pow-response header value for a solved challenge. */
function buildHeader(ch, answer, targetPath) {
  const payload = {
    algorithm: ch.algorithm,
    challenge: ch.challenge,
    salt: ch.salt,
    answer,
    signature: ch.signature,
    target_path: targetPath,
  };
  return Buffer.from(JSON.stringify(payload), 'utf8').toString('base64');
}

module.exports = { init, solve, buildHeader, Sponge, selfTest };
