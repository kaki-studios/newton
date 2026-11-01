/**
 * Conversation state for the chat tab, and the seam a real LLM backend plugs into.
 *
 * The point of `Chat` is that none of this knows about the DOM: the chat tab renders
 * `history()` and subscribes for changes, so replacing `dummyChat` with a real request
 * later does not touch the view.
 */

/** One turn: `user` text came from the input, `assistant` text from the backend. */
export interface ChatMessage {
  role: 'user' | 'assistant';
  text: string;
}

/**
 * Turns the conversation so far into the next assistant message. The real version will
 * ask an LLM for a scene document; `dummyChat` stands in until that exists.
 */
export type ChatBackend = (messages: readonly ChatMessage[]) => Promise<string>;

/** Placeholder backend: every request answers "dummy", which proves the wiring works. */
export const dummyChat: ChatBackend = async () => 'dummy';

/**
 * A conversation plus the one request in flight. `send()` rejects instead of silently
 * dropping input, so the view can tell the user what happened.
 */
export class Chat {
  private readonly messages: ChatMessage[] = [];
  private readonly listeners = new Set<() => void>();
  private pending = false;

  constructor(private readonly backend: ChatBackend) {}

  /** Copy of the conversation, oldest first: a listener cannot rewrite the log. */
  history(): ChatMessage[] {
    return this.messages.map((message) => ({ ...message }));
  }

  /** True while a reply is in flight: a second send would interleave two answers. */
  get isPending(): boolean {
    return this.pending;
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

    this.pending = true;
    this.messages.push({ role: 'user', text: message });
    this.notify();

    try {
      const reply = (await this.backend(this.history())).trim();
      // An empty bubble would look like a crashed reply; say so instead of showing nothing.
      if (!reply) throw new Error('the chat backend returned an empty reply: nothing to show');
      this.messages.push({ role: 'assistant', text: reply });
    } finally {
      this.pending = false;
      this.notify();
    }
  }

  private notify(): void {
    for (const listener of this.listeners) listener();
  }
}
