'use strict';
const { describe, it } = require('node:test');
const assert = require('node:assert/strict');
const { parseDSML, StreamFilter } = require('./dsml');

const FW = '｜';
const open = (tag, attrs = '') => `<${FW}DSML${FW}${tag}${attrs}>`;
const close = (tag) => `</${FW}DSML${FW}${tag}>`;
const wrap = (inner) => open('calls') + '\n' + inner + '\n' + close('calls');

describe('dsml basic', () => {
  it('one invoke', () => {
    const raw = wrap(open('invoke', ' name="bash"') + open('parameter', ' name="command" string="true"') + '\nls -la /home/duox/IdeaProjects\n' + close('parameter') + close('invoke'));
    const r = parseDSML(raw);
    assert.equal(r.calls.length, 1);
    assert.equal(r.calls[0].name, 'bash');
    assert.equal(r.calls[0].arguments.command, 'ls -la /home/duox/IdeaProjects');
    assert.equal(r.cleanText, '');
  });
  it('multiple invokes + params, empty, multiline', () => {
    const raw = wrap(
      open('invoke', ' name="bash"') + open('parameter', ' name="command"') + 'pwd' + close('parameter') + close('invoke') +
      open('invoke', ' name="edit"') + open('parameter', ' name="a"') + '' + close('parameter') +
      open('parameter', ' name="b"') + '\nline1\nline2\n' + close('parameter') + close('invoke'));
    const r = parseDSML(raw);
    assert.equal(r.calls.length, 2);
    assert.equal(r.calls[1].arguments.a, '');
    assert.equal(r.calls[1].arguments.b, 'line1\nline2');
  });
  it('mixed content', () => {
    const dsml = wrap(open('invoke', ' name="bash"') + open('parameter', ' name="command"') + 'pwd' + close('parameter') + close('invoke'));
    assert.equal(parseDSML('Hello').cleanText, 'Hello');
    assert.equal(parseDSML('Hi\n\n' + dsml).cleanText, 'Hi');
    assert.equal(parseDSML(dsml + '\nDone.').cleanText, 'Done.');
  });
  it('invalid fails safe', () => {
    assert.equal(parseDSML(open('calls') + 'oops').calls.length, 0);
    assert.equal(parseDSML('a < b').calls.length, 0);
  });
  it('ascii fallback', () => {
    const r = parseDSML('<|DSML|calls><|DSML|invoke name="bash"><|DSML|parameter name="command">pwd</|DSML|parameter></|DSML|invoke></|DSML|calls>');
    assert.equal(r.calls.length, 1);
  });
  it('doubled bars (live session shape)', () => {
    const BB = FW + FW;
    const raw = `<${BB}DSML${BB} calls>\n` +
      `<${BB}DSML${BB} invoke name="read">\n` +
      `<${BB}DSML${BB} parameter name="i" string="true">Reading main Go entrypoint</${BB}DSML${BB} parameter>\n` +
      `<${BB}DSML${BB} parameter name="path" string="true">main.go</${BB}DSML${BB} parameter>\n` +
      `</${BB}DSML${BB} invoke>\n` +
      `</${BB}DSML${BB} calls>`;
    const r = parseDSML(raw);
    assert.equal(r.calls.length, 1);
    assert.equal(r.calls[0].name, 'read');
    assert.equal(r.calls[0].arguments.path, 'main.go');
    assert.equal(r.cleanText, '');
    const f = new StreamFilter();
    let safe = '';
    for (let i = 0; i < raw.length; i += 2) safe += f.write(raw.slice(i, i + 2));
    const { rest, calls } = f.flush();
    safe += rest;
    assert.equal(calls.length, 1);
    assert.ok(!safe.includes('DSML'), safe);
  });
  it('streaming split', () => {
    const full = 'Hi\n' + wrap(open('invoke', ' name="bash"') + open('parameter', ' name="command"') + 'ls -la' + close('parameter') + close('invoke'));
    for (const size of [1, 3, 7]) {
      const f = new StreamFilter();
      let safe = '';
      for (let i = 0; i < full.length; i += size) safe += f.write(full.slice(i, i + size));
      const { rest, calls } = f.flush();
      safe += rest;
      assert.equal(calls.length, 1, `size=${size}`);
      assert.ok(!safe.includes('DSML'), safe);
    }
  });
});
