/**
 * `POST /api/chat` over `fetch`, read as a stream of SSE frames.
 *
 * The server decides refusals before the first byte (HTTP status + `{"error": "..."}`) and reports
 * failures after it as `event: error` frames. Both end up as one `ChatError` with a stable `code`,
 * so the view has a single thing to switch on, which is what the server's comment promises: "the
 * client treats both identically".
 */

import { createSseParser } from './sse.js';
import {
  isDeltaEvent,
  isDoneEvent,
  isErrorEvent,
  type ChatRequest,
  type DoneEvent,
  type ErrorCode,
} from './protocol.js';

/** The wire codes, plus what only the client can observe. */
export type ChatErrorCode =
  | ErrorCode
  /** 401: the session expired or was revoked. */
  | 'unauthorized'
  /** 503: the operator's kill switch. */
  | 'paused'
  /** 400/413/415: a client bug; the message says which field. */
  | 'bad_request'
  /** `fetch` itself rejected: offline, DNS, refused connection. */
  | 'network'
  /** The stream ended without `done` or `error`. */
  | 'connection_lost';

export class ChatError extends Error {
  constructor(
    readonly code: ChatErrorCode,
    message: string,
    /** The HTTP status for a refusal before the stream started. */
    readonly status?: number,
    /** Seconds, from `Retry-After` on a 409 or 429. */
    readonly retryAfter?: number,
  ) {
    super(message);
    this.name = 'ChatError';
  }
}

export interface StreamOptions {
  fetch?: typeof fetch;
  signal?: AbortSignal;
  /** Each fragment of the reply, in order. Fragments align with nothing; concatenate them. */
  onDelta(text: string): void;
}

/** Send one turn and stream the reply. Resolves with `done`; rejects with `ChatError`. */
export async function streamChat(request: ChatRequest, options: StreamOptions): Promise<DoneEvent> {
  const fetchImpl = options.fetch ?? fetch;
  const { signal } = options;

  let response: Response;
  try {
    response = await fetchImpl('/api/chat', {
      method: 'POST',
      // JSON-only is the CSRF defence on the server, so the header is not optional.
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body: JSON.stringify(request),
      credentials: 'same-origin',
      signal,
    });
  } catch (error) {
    throw signal?.aborted ? canceled() : new ChatError('network', `could not reach the server: ${describe(error)}`);
  }

  if (!response.ok) throw await refusal(response);

  const type = response.headers.get('Content-Type') ?? '';
  if (!type.startsWith('text/event-stream') || !response.body) {
    throw new ChatError('internal', `the server answered ${type || 'no content type'} instead of an event stream`);
  }

  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  const parser = createSseParser();
  try {
    for (;;) {
      let chunk: ReadableStreamReadResult<string>;
      try {
        chunk = await reader.read();
      } catch (error) {
        throw signal?.aborted ? canceled() : lost(describe(error));
      }
      if (chunk.done) {
        parser.end();
        throw signal?.aborted ? canceled() : lost('the stream ended before the reply finished');
      }

      for (const frame of parser.push(chunk.value)) {
        let payload: unknown;
        try {
          payload = JSON.parse(frame.data);
        } catch {
          throw new ChatError('internal', `the server sent a ${frame.event} frame that is not JSON`);
        }

        if (frame.event === 'delta') {
          if (!isDeltaEvent(payload)) throw malformed('delta');
          options.onDelta(payload.text);
        } else if (frame.event === 'done') {
          if (!isDoneEvent(payload)) throw malformed('done');
          return payload;
        } else if (frame.event === 'error') {
          if (!isErrorEvent(payload)) throw malformed('error');
          throw new ChatError(payload.code, payload.message);
        }
        // Unknown event names are a newer server talking to an older page: skip, do not fail.
      }
    }
  } finally {
    // Releases the connection on every exit, including `done` arriving before EOF.
    reader.cancel().catch(() => {});
  }
}

/** Map a pre-stream refusal to a code. The server's sentence is shown as is when it sent one. */
async function refusal(response: Response): Promise<ChatError> {
  const { status } = response;
  const code: ChatErrorCode =
    status === 401
      ? 'unauthorized'
      : status === 409
        ? 'in_flight'
        : status === 429
          ? 'quota_exceeded'
          : status === 503
            ? 'paused'
            : status === 400 || status === 413 || status === 415
              ? 'bad_request'
              : 'internal';

  let message = `the server refused the request: ${status} ${response.statusText}`.trim();
  try {
    const body: unknown = await response.json();
    if (typeof body === 'object' && body !== null && typeof (body as { error?: unknown }).error === 'string') {
      message = (body as { error: string }).error;
    }
  } catch {
    // Not JSON (a proxy's page, or python's http.server answering 501): keep the status line.
  }

  const retry = Number(response.headers.get('Retry-After'));
  return new ChatError(code, message, status, Number.isFinite(retry) && retry > 0 ? retry : undefined);
}

function canceled(): ChatError {
  return new ChatError('canceled', 'stopped: the reply was cancelled');
}

function lost(detail: string): ChatError {
  return new ChatError('connection_lost', `connection lost: ${detail}. The text so far is kept; send again to retry`);
}

function malformed(event: string): ChatError {
  return new ChatError('internal', `the server sent a ${event} frame with unexpected fields`);
}

function describe(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
