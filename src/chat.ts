/**
 * Conversation state for the chat tab, and the seam a real LLM backend plugs into.
 *
 * The point of `Chat` is that none of this knows about the DOM: the chat tab renders
 * `history()` and subscribes for changes, so swapping `dummyChat` for the streaming client
 * (`src/ai/glue.ts`) or the canned `fakeStream` does not touch the view.
 */

import type { SceneIssue } from './scene/error.js';
import { MAX_ISSUES, type Report } from './ai/protocol.js';

/**
 * What the validator said about an assistant reply. `document` is the extracted JSON text, so
 * the view can put exactly what was validated into the editor.
 */
export type Verdict = { ok: true; document: string } | { ok: false; issues: SceneIssue[] };

/**
 * One turn: `user` text came from the input, `assistant` text from the backend. The optional
 * fields are absent, not `undefined`, when they do not apply, so a plain turn stays `{role, text}`.
 */
export interface ChatMessage {
  role: 'user' | 'assistant';
  text: string;
  /** User turns only: the previous verdict, sent so the server can render a repair prompt. */
  report?: Report;
  /** Assistant turns only: `streaming` while deltas arrive, `failed` when the stream broke. */
  status?: 'streaming' | 'failed';
  /** Assistant turns only, and only when the chat has a `judge`. */
  verdict?: Verdict;
}

/** Where a streaming backend puts each fragment as it arrives. */
export interface DeltaSink {
  delta(text: string): void;
}

/**
 * Turns the conversation so far into the next assistant message. A streaming backend pushes
 * fragments into `sink` as they arrive and resolves with the full text; a non-streaming one
 * ignores the sink and just resolves. `signal` aborts when the user presses Stop.
 */
export type ChatBackend = (
  messages: readonly ChatMessage[],
  sink: DeltaSink,
  signal: AbortSignal,
) => Promise<string>;

/** Placeholder backend: every request answers "dummy", which proves the wiring works. */
export const dummyChat: ChatBackend = async () => 'dummy';

/** The user-visible text of a repair turn. The instruction itself is composed by the server. */
export const REPAIR_TEXT = 'Fix the reported issues.';

export interface ChatOptions {
  /** Runs once per finished reply. `chat.ts` stays free of `src/scene` by taking it as a value. */
  judge?: (text: string) => Verdict;
}

/**
 * A conversation plus the one request in flight. `send()` rejects instead of silently
 * dropping input, so the view can tell the user what happened.
 */
export class Chat {
  private messages: ChatMessage[] = [];
  private readonly listeners = new Set<() => void>();
  private pending: AbortController | null = null;

  constructor(
    private readonly backend: ChatBackend,
    private readonly options: ChatOptions = {},
  ) {}

  /** Deep copy of the conversation, oldest first: a listener cannot rewrite the log. */
  history(): ChatMessage[] {
    return structuredClone(this.messages);
  }

  /** True while a reply is in flight: a second send would interleave two answers. */
  get isPending(): boolean {
    return this.pending !== null;
  }

  /** True when the newest reply failed validation, i.e. `repair()` has something to send. */
  get canRepair(): boolean {
    const last = this.messages.at(-1);
    return !this.pending && last?.role === 'assistant' && last.verdict?.ok === false;
  }

  /** Runs `listener` after every change; returns the unsubscribe function. */
  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  /**
   * Appends `text` as a user turn, then the backend's answer. A blank message is not a
   * turn. A failed request keeps the user turn, so a retry does not lose what was asked.
   */
  async send(text: string): Promise<void> {
    if (this.pending) throw new Error('a reply is already pending: wait for it before sending again');
    const message = text.trim();
    if (!message) return;
    await this.turn({ role: 'user', text: message });
  }

  /** Ask the model to fix the newest reply, sending its issues as the report. */
  async repair(): Promise<void> {
    if (this.pending) throw new Error('a reply is already pending: wait for it before sending again');
    const last = this.messages.at(-1);
    if (last?.role !== 'assistant' || last.verdict?.ok !== false) {
      throw new Error('there is nothing to repair: the newest reply was not rejected by the validator');
    }
    await this.turn({
      role: 'user',
      text: REPAIR_TEXT,
      report: { ok: false, issues: last.verdict.issues.slice(0, MAX_ISSUES) },
    });
  }

  /** Abort the reply in flight. The backend rejects, and `send` reports it like any failure. */
  cancel(): void {
    this.pending?.abort();
  }

  /** Start over. Refused while a reply is streaming, because its deltas would land nowhere. */
  reset(): void {
    if (this.pending) throw new Error('a reply is still streaming: stop it before starting a new conversation');
    this.messages = [];
    this.notify();
  }

  private async turn(user: ChatMessage): Promise<void> {
    const controller = new AbortController();
    this.pending = controller;
    this.messages.push(user);
    this.notify();

    // The assistant row appears with the first fragment, not before: until then there is
    // nothing to show, and `aria-busy` on the log already says a reply is coming.
    // `as`, not an annotation: TS would narrow a plain `null` and miss the closure assigning it.
    let reply = null as ChatMessage | null;
    const sink: DeltaSink = {
      delta: (fragment) => {
        if (controller.signal.aborted || !fragment) return;
        if (!reply) {
          reply = { role: 'assistant', text: '', status: 'streaming' };
          this.messages.push(reply);
        }
        reply.text += fragment;
        this.notify();
      },
    };

    try {
      let text: string;
      try {
        text = (await this.backend(this.history(), sink, controller.signal)).trim();
      } catch (error) {
        this.fail(reply);
        throw error;
      }
      // An empty bubble would look like a crashed reply; say so instead of showing nothing.
      if (!text) {
        this.fail(reply);
        throw new Error('the chat backend returned an empty reply: nothing to show');
      }

      const done: ChatMessage = reply ?? { role: 'assistant', text };
      if (!reply) this.messages.push(done);
      done.text = text;
      delete done.status;
      if (this.options.judge) done.verdict = this.options.judge(text);
    } finally {
      this.pending = null;
      this.notify();
    }
  }

  /** A broken stream keeps what arrived (it is still worth reading); an empty one leaves no row. */
  private fail(reply: ChatMessage | null): void {
    if (!reply) return;
    if (reply.text.trim()) reply.status = 'failed';
    else this.messages.splice(this.messages.indexOf(reply), 1);
  }

  private notify(): void {
    for (const listener of this.listeners) listener();
  }
}
