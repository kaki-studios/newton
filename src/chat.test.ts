import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Chat, dummyChat, type ChatMessage } from './chat.js';

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
