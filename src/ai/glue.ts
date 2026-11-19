/**
 * The verdict: what happens to a reply once it has finished streaming, and the backend that
 * talks to the real server.
 *
 * `judge` is the only place a reply meets the validator, and it is the same `buildSceneJson` the
 * Scene JSON tab runs on Apply — one copy of the schema, in the browser, as the server promises.
 */

import { buildSceneJson, SceneError, type SceneIssue } from '../scene/index.js';
import type { ChatBackend, Verdict } from '../chat.js';
import { extractJsonBlock } from './jsonblock.js';
import { MAX_SCENE_BYTES, type ChatRequest, type DoneEvent } from './protocol.js';
import type { StreamOptions } from './client.js';

/** Extract the document and validate it. Never throws: a bad reply is a verdict, not a crash. */
export function judge(text: string): Verdict {
  let document: string;
  try {
    document = extractJsonBlock(text);
    buildSceneJson(document);
  } catch (error) {
    return { ok: false, issues: issuesOf(error) };
  }
  return { ok: true, document };
}

/** The two things the editor lets the AI do. `main.ts` passes the textarea and its Apply. */
export interface Editor {
  setText(text: string): void;
  apply(): void;
}

/**
 * Valid: put the document in the editor and apply it, so the user can see and edit exactly what
 * runs. Invalid: touch nothing — the old scene keeps playing and the textarea keeps the user's text.
 */
export function deliver(verdict: Verdict, editor: Editor): void {
  if (!verdict.ok) return;
  editor.setText(verdict.document);
  editor.apply();
}

export interface AiBackendOptions {
  /** `streamChat`, injected so tests do not need a server. */
  stream: (request: ChatRequest, options: StreamOptions) => Promise<DoneEvent>;
  /** The textarea text at send time: "make the ramp steeper" needs the current ramp. */
  getScene: () => string;
  /** Called after `done`, e.g. to re-read `/api/me` so the quota counter is current. */
  onDone?: (done: DoneEvent) => void;
}

/**
 * The `ChatBackend` for `POST /api/chat`. The server holds no transcript; continuity is the
 * conversation id it mints on the first turn, which this closure carries forward.
 *
 * A history of one message is a new conversation (`Chat.reset()` was called, or this is the first
 * turn), so the id is dropped then. That keeps "New conversation" a single call on `Chat`.
 */
export function aiBackend(options: AiBackendOptions): ChatBackend {
  let conversationId: string | null = null;

  return async (messages, sink, signal) => {
    const last = messages.at(-1);
    if (last?.role !== 'user') throw new Error('the chat backend was called without a user turn to answer');
    if (messages.length === 1) conversationId = null;

    const request: ChatRequest = { message: last.text };
    if (conversationId) request.conversationId = conversationId;
    const scene = options.getScene();
    // An oversized scene would make the whole turn a 400; without it the model starts fresh, which
    // is a worse answer but still an answer.
    if (scene.trim() && new TextEncoder().encode(scene).length <= MAX_SCENE_BYTES) request.scene = scene;
    if (last.report) request.report = last.report;

    let text = '';
    const done = await options.stream(request, {
      signal,
      onDelta: (fragment) => {
        text += fragment;
        sink.delta(fragment);
      },
    });
    conversationId = done.conversationId;
    options.onDone?.(done);
    return text;
  };
}

function issuesOf(error: unknown): SceneIssue[] {
  if (error instanceof SceneError) return error.issues;
  return [{ path: '', message: error instanceof Error ? error.message : String(error) }];
}
