import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { aiBackend, deliver, judge } from './glue.js';
import { fakeStream, PENDULUM } from './fake.js';
import { Chat, REPAIR_TEXT, type ChatMessage } from '../chat.js';
import type { ChatRequest, DoneEvent } from './protocol.js';
import type { StreamOptions } from './client.js';

/** A textarea and an Apply button, minus the DOM. */
function editor(initial = 'old text') {
  const log: string[] = [];
  const state = { text: initial };
  return {
    state,
    log,
    setText(text: string) {
      state.text = text;
      log.push('setText');
    },
    apply() {
      log.push('apply');
    },
  };
}

const BAD = '```json\n{"bodies":[{"id":"ball","type":"dynamic","position":{"x":0,"y":0},"shape":{"kind":"circle","radius":-1}}]}\n```';

test('the fake stream and the Go fake provider stream the same, valid pendulum', () => {
  const fixture = readFileSync(new URL('../../testdata/sse/happy-path.txt', import.meta.url), 'utf8');
  const text = JSON.parse(fixture.split('\n')[1]!.slice('data: '.length)).text;
  assert.equal(PENDULUM, text);
  assert.equal(judge(PENDULUM).ok, true);
});

test('a valid reply goes into the editor and is applied, in that order', () => {
  const target = editor();
  const verdict = judge(PENDULUM);
  deliver(verdict, target);

  assert.deepEqual(target.log, ['setText', 'apply']);
  assert.match(target.state.text, /^\{\n  "gravity"/);
  assert.doesNotMatch(target.state.text, /```/);
});

test('an invalid reply touches nothing and reports every path', () => {
  const target = editor();
  const verdict = judge(BAD);
  deliver(verdict, target);

  assert.deepEqual(target.log, []);
  assert.equal(target.state.text, 'old text');
  assert.equal(verdict.ok, false);
  assert.ok(!verdict.ok && verdict.issues.some((i) => i.path === 'bodies[0].shape.radius'));
});

test('a reply with no document is a verdict, not a crash', () => {
  const verdict = judge('Sorry, I can only describe physical systems.');
  assert.equal(verdict.ok, false);
  assert.match(!verdict.ok ? verdict.issues[0]!.message : '', /no ```json block/);
});

/** A `streamChat` stand-in that records requests and streams `reply`. */
function recordingStream(reply = PENDULUM) {
  const requests: ChatRequest[] = [];
  let n = 0;
  const stream = async (request: ChatRequest, options: StreamOptions): Promise<DoneEvent> => {
    requests.push(structuredClone(request));
    for (const piece of reply.match(/[\s\S]{1,10}/g) ?? []) options.onDelta(piece);
    n++;
    return { conversationId: 'conv-1', generationId: `gen-${n}`, usage: { input: 1, output: 1 } };
  };
  return { stream, requests };
}

test('the conversation id from done is carried to the next turn and dropped after reset', async () => {
  const { stream, requests } = recordingStream();
  const chat = new Chat(aiBackend({ stream, getScene: () => '' }));

  await chat.send('a pendulum');
  await chat.send('make it longer');
  chat.reset();
  await chat.send('a ramp');

  assert.deepEqual(
    requests.map((r) => r.conversationId),
    [undefined, 'conv-1', undefined],
  );
});

test('the scene is sent when it fits, and omitted (not truncated) when it does not', async () => {
  const { stream, requests } = recordingStream();
  let scene = '{"bodies": []}';
  const chat = new Chat(aiBackend({ stream, getScene: () => scene }));

  await chat.send('one');
  scene = 'x'.repeat(64 * 1024 + 1);
  await chat.send('two');
  scene = '   ';
  await chat.send('three');

  assert.equal(requests[0]!.scene, '{"bodies": []}');
  assert.equal('scene' in requests[1]!, false);
  assert.equal('scene' in requests[2]!, false);
});

test('the reply streams into one row, gets a verdict, and onDone fires', async () => {
  const { stream } = recordingStream();
  const done: string[] = [];
  const chat = new Chat(aiBackend({ stream, getScene: () => '', onDone: (d) => done.push(d.generationId) }), {
    judge,
  });
  const lengths: number[] = [];
  chat.subscribe(() => lengths.push(chat.history().at(-1)!.text.length));

  await chat.send('a pendulum');

  const history = chat.history();
  assert.equal(history.length, 2);
  assert.deepEqual(history[1]!.verdict, judge(PENDULUM));
  assert.deepEqual(done, ['gen-1']);
  // The assistant row grew fragment by fragment rather than appearing whole.
  assert.ok(new Set(lengths).size > 5, `lengths: ${lengths.join(',')}`);
});

test('the repair loop: an invalid reply, then a repair turn carrying its issues as the report', async () => {
  const { stream, requests } = recordingStream(BAD);
  const chat = new Chat(aiBackend({ stream, getScene: () => '' }), { judge });

  await chat.send('a ball');
  assert.equal(chat.canRepair, true);
  await chat.repair();

  const repair = requests[1]!;
  assert.equal(repair.message, REPAIR_TEXT);
  assert.equal(repair.conversationId, 'conv-1');
  assert.equal(repair.report?.ok, false);
  assert.ok(repair.report?.issues.some((i) => i.path === 'bodies[0].shape.radius'));
});

test('fakeStream honours Stop between fragments', async () => {
  const chat = new Chat(fakeStream(PENDULUM, { chunk: 4, delayMs: 1 }), { judge });
  const sending = chat.send('a pendulum');
  chat.subscribe(() => {
    if ((chat.history().at(-1) as ChatMessage).role === 'assistant') chat.cancel();
  });

  await assert.rejects(sending, /stopped/);
  const last = chat.history().at(-1)!;
  assert.equal(last.status, 'failed');
  assert.equal(last.verdict, undefined, 'a cut-off reply is not judged');
});
