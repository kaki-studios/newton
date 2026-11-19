import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { ChatError, streamChat } from './client.js';

const FIXTURES = new URL('../../testdata/sse/', import.meta.url);

/** A ReadableStream of `chunks`, which errors like a real fetch body when `signal` aborts. */
function body(chunks: string[], signal?: AbortSignal | null, hang = false): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      if (!hang) controller.close();
      signal?.addEventListener('abort', () => controller.error(new DOMException('aborted', 'AbortError')));
    },
  });
}

interface Call {
  url: string;
  init: RequestInit;
}

function stubFetch(respond: (init: RequestInit) => Response) {
  const calls: Call[] = [];
  const impl = (async (url: string, init: RequestInit) => {
    calls.push({ url, init });
    return respond(init);
  }) as unknown as typeof fetch;
  return { impl, calls };
}

function sse(chunks: string[], init?: RequestInit, hang = false): Response {
  return new Response(body(chunks, init?.signal, hang), {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  });
}

function fixture(name: string): string {
  return readFileSync(new URL(name, FIXTURES), 'utf8');
}

async function rejection(promise: Promise<unknown>): Promise<ChatError> {
  try {
    await promise;
  } catch (error) {
    assert.ok(error instanceof ChatError, `expected a ChatError, got ${String(error)}`);
    return error;
  }
  assert.fail('expected the stream to reject');
}

test('the request is a same-origin JSON POST, because JSON-only is the CSRF defence', async () => {
  const { impl, calls } = stubFetch((init) => sse([fixture('happy-path.txt')], init));
  await streamChat({ message: 'a pendulum' }, { fetch: impl, onDelta: () => {} });

  assert.equal(calls[0]!.url, '/api/chat');
  assert.equal(calls[0]!.init.method, 'POST');
  assert.equal(calls[0]!.init.credentials, 'same-origin');
  assert.deepEqual(calls[0]!.init.headers, { 'Content-Type': 'application/json', Accept: 'text/event-stream' });
  assert.deepEqual(JSON.parse(String(calls[0]!.init.body)), { message: 'a pendulum' });
});

test('keepalive.txt split mid-frame: deltas in order, resolves with done', async () => {
  const text = fixture('keepalive.txt');
  // Split mid-frame to prove the parser, not the chunking, is what finds the events.
  const { impl } = stubFetch((init) => sse([text.slice(0, 7), text.slice(7, 40), text.slice(40)], init));
  const deltas: string[] = [];

  const done = await streamChat({ message: 'x' }, { fetch: impl, onDelta: (t) => deltas.push(t) });

  assert.deepEqual(deltas, ['first', 'second']);
  assert.equal(done.conversationId, 'conv-fixture');
});

test('provider-error.txt: the deltas arrive, then the wire code rejects', async () => {
  const { impl } = stubFetch((init) => sse([fixture('provider-error.txt')], init));
  const deltas: string[] = [];

  const error = await rejection(streamChat({ message: 'x' }, { fetch: impl, onDelta: (t) => deltas.push(t) }));

  assert.deepEqual(deltas, ['partial reply']);
  assert.equal(error.code, 'provider_overloaded');
  assert.equal(error.message, 'The AI is busy. Try again in a minute.');
});

test('refusals before the stream map to codes and keep the server sentence', async () => {
  const cases: [number, string][] = [
    [401, 'unauthorized'],
    [409, 'in_flight'],
    [429, 'quota_exceeded'],
    [503, 'paused'],
    [415, 'bad_request'],
    [400, 'bad_request'],
    [500, 'internal'],
  ];
  for (const [status, code] of cases) {
    const { impl } = stubFetch(
      () =>
        new Response(JSON.stringify({ error: `sentence for ${status}` }), {
          status,
          headers: { 'Content-Type': 'application/json', 'Retry-After': '30' },
        }),
    );
    const error = await rejection(streamChat({ message: 'x' }, { fetch: impl, onDelta: () => {} }));
    assert.equal(error.code, code, `status ${status}`);
    assert.equal(error.status, status);
    assert.equal(error.message, `sentence for ${status}`);
    assert.equal(error.retryAfter, 30);
  }
});

test('a non-JSON refusal (python http.server answering 501) still says what happened', async () => {
  const { impl } = stubFetch(() => new Response('<html>Unsupported method</html>', { status: 501, statusText: 'Not Implemented' }));
  const error = await rejection(streamChat({ message: 'x' }, { fetch: impl, onDelta: () => {} }));
  assert.equal(error.code, 'internal');
  assert.match(error.message, /501 Not Implemented/);
});

test('a 200 that is not an event stream is refused rather than parsed', async () => {
  const { impl } = stubFetch(() => new Response('{}', { headers: { 'Content-Type': 'application/json' } }));
  const error = await rejection(streamChat({ message: 'x' }, { fetch: impl, onDelta: () => {} }));
  assert.match(error.message, /instead of an event stream/);
});

test('a stream that ends without done or error is a lost connection', async () => {
  const { impl } = stubFetch((init) => sse(['event: delta\ndata: {"text":"half"}\n\n'], init));
  const error = await rejection(streamChat({ message: 'x' }, { fetch: impl, onDelta: () => {} }));
  assert.equal(error.code, 'connection_lost');
});

test('a fetch that rejects is a network error', async () => {
  const impl = (async () => {
    throw new TypeError('Failed to fetch');
  }) as unknown as typeof fetch;
  const error = await rejection(streamChat({ message: 'x' }, { fetch: impl, onDelta: () => {} }));
  assert.equal(error.code, 'network');
  assert.match(error.message, /Failed to fetch/);
});

test('aborting mid-stream rejects as canceled and the signal reaches fetch', async () => {
  const controller = new AbortController();
  const { impl, calls } = stubFetch((init) => sse(['event: delta\ndata: {"text":"a"}\n\n'], init, true));
  const deltas: string[] = [];

  const streaming = streamChat(
    { message: 'x' },
    {
      fetch: impl,
      signal: controller.signal,
      onDelta: (t) => {
        deltas.push(t);
        controller.abort();
      },
    },
  );
  const error = await rejection(streaming);

  assert.equal(calls[0]!.init.signal, controller.signal);
  assert.deepEqual(deltas, ['a']);
  assert.equal(error.code, 'canceled');
});

test('a frame with the wrong fields is reported, not rendered as "undefined"', async () => {
  const { impl } = stubFetch((init) => sse(['event: delta\ndata: {"txt":"a"}\n\n'], init));
  const error = await rejection(streamChat({ message: 'x' }, { fetch: impl, onDelta: () => {} }));
  assert.match(error.message, /delta frame with unexpected fields/);
});
