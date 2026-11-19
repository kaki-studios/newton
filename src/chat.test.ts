import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Chat, dummyChat, REPAIR_TEXT, type ChatBackend, type ChatMessage, type DeltaSink } from './chat.js';

/** A backend that answers only when the test releases it, to observe the pending state. */
function deferredBackend() {
  let release: (reply: string) => void = () => {};
  const reply = new Promise<string>((resolve) => {
    release = resolve;
  });
  return { chat: new Chat(() => reply), release };
}

test('the dummy backend answers every message with "dummy"', async () => {
  const chat = new Chat(dummyChat);
  await chat.send('a ball on a string');

  assert.deepEqual(chat.history(), [
    { role: 'user', text: 'a ball on a string' },
    { role: 'assistant', text: 'dummy' },
  ]);
  assert.equal(chat.isPending, false);
});

test('each request carries the whole conversation, not just the newest line', async () => {
  const seen: ChatMessage[][] = [];
  const chat = new Chat(async (messages) => {
    seen.push([...messages]);
    return 'dummy';
  });

  await chat.send('one');
  await chat.send('two');

  assert.deepEqual(seen[1], [
    { role: 'user', text: 'one' },
    { role: 'assistant', text: 'dummy' },
    { role: 'user', text: 'two' },
  ]);
});

test('a blank message is not a turn and never reaches the backend', async () => {
  let calls = 0;
  const chat = new Chat(async () => {
    calls++;
    return 'dummy';
  });

  await chat.send('   \n ');

  assert.equal(calls, 0);
  assert.deepEqual(chat.history(), []);
});

test('a send during a pending reply is refused with an actionable reason', async () => {
  const { chat, release } = deferredBackend();

  const first = chat.send('one');
  await assert.rejects(chat.send('two'), /already pending: wait for it before sending again/);

  release('dummy');
  await first;
  assert.deepEqual(
    chat.history().map((message) => message.text),
    ['one', 'dummy'],
  );
});

test('a failed request keeps the user turn and clears the pending state for a retry', async () => {
  let attempts = 0;
  const chat = new Chat(async () => {
    attempts++;
    if (attempts === 1) throw new Error('backend unreachable: check the network');
    return 'dummy';
  });

  await assert.rejects(chat.send('hello'), /backend unreachable/);

  assert.deepEqual(chat.history(), [{ role: 'user', text: 'hello' }]);
  assert.equal(chat.isPending, false);

  await chat.send('again');
  assert.deepEqual(
    chat.history().map((message) => message.role),
    ['user', 'user', 'assistant'],
  );
});

test('an empty reply is rejected, so the chat never shows a blank bubble', async () => {
  const chat = new Chat(async () => '   ');

  await assert.rejects(chat.send('hello'), /returned an empty reply: nothing to show/);
  assert.deepEqual(chat.history(), [{ role: 'user', text: 'hello' }]);
});

test('listeners see the user turn immediately and the pending flag clear afterwards', async () => {
  const chat = new Chat(async () => 'dummy');
  const states: string[] = [];
  const unsubscribe = chat.subscribe(() => states.push(`${chat.isPending}:${chat.history().length}`));

  const sending = chat.send('hi');
  // send() starts the backend synchronously, so the user turn exists before the await.
  assert.deepEqual(states, ['true:1']);

  await sending;
  assert.deepEqual(states, ['true:1', 'false:2']);

  unsubscribe();
  await chat.send('again');
  assert.equal(states.length, 2);
});

test('history is a copy, so the view cannot corrupt the conversation', async () => {
  const chat = new Chat(dummyChat);
  await chat.send('hi');

  const history = chat.history();
  history.push({ role: 'user', text: 'sneaked in' });
  history[0]!.text = 'rewritten';

  assert.deepEqual(
    chat.history().map((message) => message.text),
    ['hi', 'dummy'],
  );
});

// Streaming: the seam `src/ai/glue.ts` plugs into.

/** A backend whose fragments the test pushes one at a time, then finishes or fails. */
function manualStream() {
  let sink: DeltaSink | null = null;
  let finish: (text: string) => void = () => {};
  let fail: (error: Error) => void = () => {};
  let signal: AbortSignal | null = null;
  const backend: ChatBackend = (_messages, s, sig) => {
    sink = s;
    signal = sig;
    return new Promise<string>((resolve, reject) => {
      finish = resolve;
      fail = reject;
      sig.addEventListener('abort', () => reject(new Error('stopped')));
    });
  };
  return {
    backend,
    delta: (text: string) => sink!.delta(text),
    finish: (text: string) => finish(text),
    fail: (error: Error) => fail(error),
    signal: () => signal!,
  };
}

test('deltas grow a single streaming row, which settles to a plain turn', async () => {
  const stream = manualStream();
  const chat = new Chat(stream.backend);
  const sending = chat.send('hi');

  stream.delta('Hel');
  assert.deepEqual(chat.history()[1], { role: 'assistant', text: 'Hel', status: 'streaming' });
  stream.delta('lo');
  assert.equal(chat.history().length, 2);
  assert.equal(chat.history()[1]!.text, 'Hello');

  stream.finish('Hello');
  await sending;
  assert.deepEqual(chat.history()[1], { role: 'assistant', text: 'Hello' });
});

test('a judge runs once on the finished text and its verdict lands on the reply', async () => {
  const judged: string[] = [];
  const chat = new Chat(async () => 'the reply', {
    judge: (text) => {
      judged.push(text);
      return { ok: false, issues: [{ path: 'bodies', message: 'empty' }] };
    },
  });

  await chat.send('hi');

  assert.deepEqual(judged, ['the reply']);
  assert.deepEqual(chat.history()[1]!.verdict, { ok: false, issues: [{ path: 'bodies', message: 'empty' }] });
  assert.equal(chat.canRepair, true);
});

test('a stream that breaks keeps the text so far, marked failed and unjudged', async () => {
  const stream = manualStream();
  let judged = 0;
  const chat = new Chat(stream.backend, { judge: () => (judged++, { ok: true, document: '' }) });
  const sending = chat.send('hi');

  stream.delta('```json\n{"bod');
  stream.fail(new Error('connection lost'));
  await assert.rejects(sending, /connection lost/);

  assert.deepEqual(chat.history()[1], { role: 'assistant', text: '```json\n{"bod', status: 'failed' });
  assert.equal(judged, 0);
  assert.equal(chat.isPending, false);
});

test('cancel aborts the signal the backend was given', async () => {
  const stream = manualStream();
  const chat = new Chat(stream.backend);
  const sending = chat.send('hi');
  stream.delta('partial');

  chat.cancel();

  assert.equal(stream.signal().aborted, true);
  await assert.rejects(sending, /stopped/);
  assert.equal(chat.history()[1]!.status, 'failed');
});

test('repair sends the previous issues as the report, capped at the server limit', async () => {
  const issues = Array.from({ length: 25 }, (_, i) => ({ path: `bodies[${i}]`, message: 'bad' }));
  const seen: ChatMessage[][] = [];
  const chat = new Chat(
    async (messages) => {
      seen.push([...messages]);
      return 'reply';
    },
    { judge: () => ({ ok: false, issues }) },
  );

  await chat.send('hi');
  await chat.repair();

  const turn = seen[1]!.at(-1)!;
  assert.equal(turn.text, REPAIR_TEXT);
  assert.deepEqual(turn.report, { ok: false, issues: issues.slice(0, 20) });
});

test('repair is refused when there is nothing to repair', async () => {
  const chat = new Chat(async () => 'reply', { judge: () => ({ ok: true, document: '{}' }) });
  await assert.rejects(chat.repair(), /nothing to repair/);
  await chat.send('hi');
  assert.equal(chat.canRepair, false);
  await assert.rejects(chat.repair(), /nothing to repair/);
});

test('reset empties the conversation, and is refused while a reply streams', async () => {
  const stream = manualStream();
  const chat = new Chat(stream.backend);
  const sending = chat.send('hi');

  assert.throws(() => chat.reset(), /stop it before starting a new conversation/);
  stream.finish('done');
  await sending;

  chat.reset();
  assert.deepEqual(chat.history(), []);
});
