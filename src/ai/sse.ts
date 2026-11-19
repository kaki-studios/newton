/**
 * An incremental `text/event-stream` parser: bytes arrive in arbitrary chunks, events come out.
 *
 * Pure and DOM-free on purpose. `EventSource` cannot POST a JSON body, so the client reads the
 * stream through `fetch` and needs its own parser; keeping it separate from the network is what
 * lets the recorded fixtures in `testdata/sse` test it one byte at a time.
 *
 * Follows the WHATWG rules that matter here: `\n`, `\r\n` and `\r` all end a line; a blank line
 * dispatches; `:` starts a comment (the server's `: ping` keepalive); multi-line `data:` joins
 * with `\n`; and an event not terminated by a blank line when the stream ends is discarded.
 */

/** One dispatched event. `event` is `"message"` when the frame did not name one. */
export interface SseEvent {
  event: string;
  data: string;
}

export interface SseParser {
  /** Feed the next chunk; returns the events it completed, in order. */
  push(chunk: string): SseEvent[];
  /** The stream ended. Anything not yet dispatched is dropped, as the spec says. */
  end(): void;
}

export function createSseParser(): SseParser {
  let buffer = '';
  let eventName = '';
  let data: string[] = [];

  function line(text: string, out: SseEvent[]): void {
    if (text === '') {
      // A blank line with no data is a no-op, which is exactly what a comment-only block is.
      if (data.length > 0) out.push({ event: eventName || 'message', data: data.join('\n') });
      eventName = '';
      data = [];
      return;
    }
    if (text.startsWith(':')) return;

    const colon = text.indexOf(':');
    const field = colon === -1 ? text : text.slice(0, colon);
    let value = colon === -1 ? '' : text.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);

    if (field === 'event') eventName = value;
    else if (field === 'data') data.push(value);
    // `id` and `retry` mean nothing to a one-shot POST stream; unknown fields are ignored by spec.
  }

  return {
    push(chunk) {
      buffer += chunk;
      const out: SseEvent[] = [];
      for (;;) {
        const end = buffer.search(/[\r\n]/);
        if (end === -1) break;
        // A trailing `\r` may be the first half of `\r\n` split across chunks: wait for more.
        if (buffer[end] === '\r' && end === buffer.length - 1) break;
        line(buffer.slice(0, end), out);
        const width = buffer[end] === '\r' && buffer[end + 1] === '\n' ? 2 : 1;
        buffer = buffer.slice(end + width);
      }
      return out;
    },
    end() {
      buffer = '';
      eventName = '';
      data = [];
    },
  };
}
