/**
 * A `ChatBackend` that streams a canned reply with no network at all.
 *
 * It is what the tests drive, and what `?ai=fake` uses so the whole loop — stream, extract,
 * validate, apply — can be watched under `npm run dev` with no Go process.
 */

import type { ChatBackend } from '../chat.js';
import { ChatError } from './client.js';

/** The same pendulum the Go fake provider streams (`server/internal/llm/fake.go`). */
export const PENDULUM =
  '```json\n' +
  `{
  "gravity": { "x": 0, "y": -9.81 },
  "bodies": [
    {
      "id": "floor",
      "type": "static",
      "position": { "x": 0, "y": -10 },
      "shape": { "kind": "edge", "a": { "x": -50, "y": 0 }, "b": { "x": 50, "y": 0 } }
    },
    {
      "id": "ball",
      "type": "dynamic",
      "position": { "x": 5, "y": 10 },
      "shape": { "kind": "circle", "radius": 0.4 }
    }
  ],
  "joints": [
    {
      "kind": "distance",
      "bodyA": "floor",
      "bodyB": "ball",
      "localAnchorA": { "x": 0, "y": 20 },
      "localAnchorB": { "x": 0, "y": 0 },
      "length": 6
    }
  ]
}` +
  '\n```\n';

export interface FakeOptions {
  /** Characters per delta. */
  chunk?: number;
  /** Pause between deltas; zero in tests, a few ms to make the demo look like a stream. */
  delayMs?: number;
}

export function fakeStream(reply: string = PENDULUM, options: FakeOptions = {}): ChatBackend {
  const chunk = Math.max(1, options.chunk ?? 16);
  const delayMs = options.delayMs ?? 0;

  return async (_messages, sink, signal) => {
    for (let i = 0; i < reply.length; i += chunk) {
      if (signal?.aborted) throw new ChatError('canceled', 'stopped: the reply was cancelled');
      sink.delta(reply.slice(i, i + chunk));
      if (delayMs > 0) await sleep(delayMs, signal);
    }
    return reply;
  };
}

function sleep(ms: number, signal: AbortSignal | undefined): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(done, ms);
    signal?.addEventListener('abort', done, { once: true });
    function done() {
      clearTimeout(timer);
      signal?.removeEventListener('abort', done);
      resolve(); // the loop's aborted check turns this into the cancellation
    }
  });
}
