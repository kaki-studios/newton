/**
 * The `POST /api/chat` wire format, mirrored from `server/internal/chat/{chat,sse}.go`.
 *
 * The recorded frames in `testdata/sse` are the contract: Go asserts it writes them byte for byte
 * and `sse.test.ts` asserts the parser reads them. Changing a name here without changing the Go
 * struct tag (and then the fixture) is a protocol break, not a refactor.
 */

import type { SceneIssue } from '../scene/error.js';

/** The previous attempt's verdict from the browser's validator. Advisory: the server never trusts it. */
export interface Report {
  ok: boolean;
  issues: SceneIssue[];
}

/** The JSON body of `POST /api/chat`. Omitted keys are omitted, never `null`. */
export interface ChatRequest {
  /** Absent on the first turn; the server mints one and returns it in `done`. */
  conversationId?: string;
  message: string;
  /** The textarea text, so "make the ramp steeper" has something to refer to. */
  scene?: string;
  report?: Report;
}

/** Limits the server enforces (`chat.go`). Checked here too, so an oversized field is dropped
 *  instead of turning the whole request into a 400. */
export const MAX_SCENE_BYTES = 64 * 1024;
export const MAX_ISSUES = 20;

export interface DeltaEvent {
  text: string;
}

export interface DoneEvent {
  conversationId: string;
  generationId: string;
  usage: { input: number; output: number };
}

/** The stable codes an `event: error` frame carries. The message is free to change; these are not. */
export type ErrorCode =
  | 'quota_exceeded'
  | 'conversation_turns'
  | 'in_flight'
  | 'provider_unavailable'
  | 'provider_overloaded'
  | 'canceled'
  | 'internal';

export interface ErrorEvent {
  code: ErrorCode;
  message: string;
}

// Frames are untrusted input like any other response body: a proxy's error page or a renamed
// field must become a reported failure, never `undefined` text in the transcript.

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

export function isDeltaEvent(value: unknown): value is DeltaEvent {
  return isRecord(value) && typeof value.text === 'string';
}

export function isDoneEvent(value: unknown): value is DoneEvent {
  if (!isRecord(value)) return false;
  if (typeof value.conversationId !== 'string' || typeof value.generationId !== 'string') return false;
  const usage = value.usage;
  return isRecord(usage) && typeof usage.input === 'number' && typeof usage.output === 'number';
}

export function isErrorEvent(value: unknown): value is ErrorEvent {
  return isRecord(value) && typeof value.code === 'string' && typeof value.message === 'string';
}
