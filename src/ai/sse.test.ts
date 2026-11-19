import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createSseParser, type SseEvent } from './sse.js';

/** `dist/ai/` → repo root. The fixtures are shared with the Go contract test. */
const FIXTURES = new URL('../../testdata/sse/', import.meta.url);

function fixture(name: string): string {
  return readFileSync(new URL(name, FIXTURES), 'utf8');
}

function parseAll(chunks: string[]): SseEvent[] {
  const parser = createSseParser();
  const events = chunks.flatMap((chunk) => parser.push(chunk));
  parser.end();
  return events;
}

/** Every split of the stream must produce the same events: frames do not align with reads. */
function bytewise(text: string): string[] {
  return [...text];
}

test('happy-path.txt: a delta carrying the whole document, then done', () => {
  const events = parseAll([fixture('happy-path.txt')]);
  assert.deepEqual(
    events.map((e) => e.event),
    ['delta', 'done'],
  );
  assert.match(JSON.parse(events[0]!.data).text, /^```json\n\{/);
  assert.deepEqual(JSON.parse(events[1]!.data), {
    conversationId: 'conv-fixture',
    generationId: 'gen-fixture-1',
    usage: { input: 2412, output: 1033 },
  });
});

test('keepalive.txt: the ping comment produces no event', () => {
  const events = parseAll([fixture('keepalive.txt')]);
  assert.deepEqual(
    events.map((e) => [e.event, e.event === 'delta' ? JSON.parse(e.data).text : null]),
    [
      ['delta', 'first'],
      ['delta', 'second'],
      ['done', null],
    ],
  );
});

test('provider-error.txt: an error frame after a delta', () => {
  const events = parseAll([fixture('provider-error.txt')]);
  assert.deepEqual(
    events.map((e) => e.event),
    ['delta', 'error'],
  );
  assert.deepEqual(JSON.parse(events[1]!.data), {
    code: 'provider_overloaded',
    message: 'The AI is busy. Try again in a minute.',
  });
});

test('every fixture parses identically when fed one character at a time', () => {
  for (const name of ['happy-path.txt', 'keepalive.txt', 'provider-error.txt']) {
    const text = fixture(name);
    assert.deepEqual(parseAll(bytewise(text)), parseAll([text]), name);
  }
});

test('CRLF line endings, including a CR and LF split across chunks', () => {
  const events = parseAll(['event: delta\r', '\ndata: {"text":"a"}\r\n\r', '\n']);
  assert.deepEqual(events, [{ event: 'delta', data: '{"text":"a"}' }]);
});

test('multi-line data joins with a newline; a nameless frame is a "message"', () => {
  assert.deepEqual(parseAll(['data: one\ndata:two\n\n']), [{ event: 'message', data: 'one\ntwo' }]);
});

test('an event without its terminating blank line is dropped at the end of the stream', () => {
  const parser = createSseParser();
  assert.deepEqual(parser.push('event: done\ndata: {}\n'), []);
  parser.end();
  assert.deepEqual(parser.push('\n'), [], 'end() discards the half-built event');
});
