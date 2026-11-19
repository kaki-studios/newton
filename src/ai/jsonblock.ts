/**
 * Find the scene document in a model reply.
 *
 * The system prompt asks for exactly one fenced ```json block, but a reply is untrusted text: it
 * may add prose, repeat itself, or stop mid-document at the token limit. The rule is strict in the
 * one place where guessing would hurt — an unterminated fence is an error, never a partial parse —
 * and forgiving where it cannot: a reply that is nothing but a bare JSON object is accepted.
 */

import { SceneError } from '../scene/error.js';

/**
 * The last complete ```json (or bare ```) block, else the whole reply when it is a bare `{…}`.
 * Throws `SceneError` so a missing document travels the same verdict path as an invalid one.
 */
export function extractJsonBlock(text: string): string {
  const blocks: string[] = [];
  let open: { json: boolean; lines: string[] } | null = null;

  for (const raw of text.split(/\r\n|\r|\n/)) {
    const trimmed = raw.trim();
    if (open) {
      if (trimmed === '```') {
        if (open.json) blocks.push(open.lines.join('\n'));
        open = null;
      } else {
        open.lines.push(raw);
      }
    } else if (trimmed.startsWith('```')) {
      const info = trimmed.slice(3).trim().toLowerCase();
      open = { json: info === '' || info === 'json', lines: [] };
    }
  }

  // The newest attempt is the one at the end. If it is cut off, an earlier complete block is a
  // stale draft, and applying it would show the user something they did not ask for.
  if (open?.json) {
    throw notFound('the reply ends inside an unclosed ```json block, so it was probably cut off at the token limit');
  }

  const last = blocks.at(-1);
  if (last !== undefined) {
    if (!last.trim()) throw notFound('the reply\'s ```json block is empty');
    return last.trim() + '\n';
  }

  const bare = text.trim();
  if (bare.startsWith('{') && bare.endsWith('}')) return bare + '\n';
  throw notFound('the reply contains no ```json block with a scene document');
}

function notFound(message: string): SceneError {
  return new SceneError('syntax', [{ path: '', message }]);
}
