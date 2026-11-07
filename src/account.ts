/**
 * Who is signed in, and what the server says about the AI.
 *
 * This is the degradation matrix from the plan in one place, because the composer's state and
 * the sign-in link have to agree about it — two independent guesses about whether the AI works
 * is how a UI ends up showing "sign in" to a signed-in user.
 *
 * Like `chat.ts`, none of this touches the DOM: `main.ts` renders, this decides.
 */

/** What `/api/me` answers. `quota.limit` is 0 when the AI is off, not "unlimited". */
export interface Account {
  id: string;
  displayName: string;
  quota: { used: number; limit: number; resetsAt: string };
  /** True while a generation is running, so a reload shows "generating…" rather than a dead button. */
  inFlight: boolean;
  /** False when the kill switch is on, which is different from "not signed in". */
  chatEnabled: boolean;
}

/** What `/api/config` answers; it works anonymously, so the sign-in link exists before login. */
export interface ServerConfig {
  chatEnabled: boolean;
  /** Sign-in providers with credentials, e.g. ["github"]. Empty means sign-in is impossible. */
  providers: string[];
  dev: boolean;
  version: string;
}

/**
 * The reasons the composer is disabled, each with the sentence the panel shows. The distinction
 * matters: "paused" is an operator decision, "unavailable" is a deployment without a backend, and
 * mixing them up tells the user to wait for something that will never happen.
 */
export type AiState =
  /** Signed in, AI on, quota left: the composer works. */
  | 'ready'
  /** Signed in, but today's quota is spent. */
  | 'quota'
  /** Signed in, but `/api/chat` answered 503: the operator paused the AI. */
  | 'paused'
  /** No session and sign-in is possible. */
  | 'signed-out'
  /** No backend at all: `npm run dev` with `python3 -m http.server`. */
  | 'unavailable'
  /** A backend, but with no OAuth provider configured. */
  | 'no-providers';

/** The full picture the view renders: state, sentence, and the sign-in URL when there is one. */
export interface AiStatus {
  state: AiState;
  /** One sentence for the user. Never empty. */
  message: string;
  /** Where to send the browser to sign in, or `null` when there is nothing to click. */
  signInUrl: string | null;
  /** The provider name for the button label, e.g. "GitHub". */
  signInLabel: string | null;
}

/** What an unconfigured or unreachable server looks like. */
const unknownConfig: ServerConfig = { chatEnabled: false, providers: [], dev: false, version: '' };

const PROVIDER_LABELS: Record<string, string> = { github: 'GitHub', google: 'Google' };

/** `/api/auth/<provider>` is the same-origin entry point the backend exposes. */
export function signInUrl(provider: string): string {
  return `/api/auth/${encodeURIComponent(provider)}`;
}

/**
 * Decide what the composer should say.
 *
 * `config` is null when `/api/config` failed, which is the static-dev case: there is no backend,
 * so the AI is not "paused", it is simply not there. `account` is null when there is a backend but
 * no session.
 */
export function aiStatus(config: ServerConfig | null, account: Account | null): AiStatus {
  const cfg = config ?? unknownConfig;

  // Order matters. No backend is the weakest claim and the one the static dev loop hits, so it is
  // checked before anything that would suggest a fix is possible.
  if (!config) {
    return {
      state: 'unavailable',
      message: 'AI unavailable in static mode (no backend)',
      signInUrl: null,
      signInLabel: null,
    };
  }

  if (cfg.providers.length === 0) {
    return {
      state: 'no-providers',
      message: 'This server has no sign-in provider configured, so the AI is off',
      signInUrl: null,
      signInLabel: null,
    };
  }

  if (!account) {
    const provider = cfg.providers[0]!;
    return {
      state: 'signed-out',
      message: `Sign in with ${PROVIDER_LABELS[provider] ?? provider} to use AI`,
      signInUrl: signInUrl(provider),
      signInLabel: `Sign in with ${PROVIDER_LABELS[provider] ?? provider}`,
    };
  }

  // `/api/config` is the authority on the kill switch: it is the public, cacheable answer, and a
  // `/api/me` fetched a moment earlier must not re-enable a composer the operator just turned off.
  // The account's own flag only matters when the config does not carry one at all.
  if (!cfg.chatEnabled || !account.chatEnabled) {
    return {
      state: 'paused',
      message: 'AI is paused by the operator',
      signInUrl: null,
      signInLabel: null,
    };
  }

  if (account.quota.used >= account.quota.limit) {
    return {
      state: 'quota',
      message: `Daily limit reached (${account.quota.used}/${account.quota.limit}). Resets ${formatReset(account.quota.resetsAt)}.`,
      signInUrl: null,
      signInLabel: null,
    };
  }

  return { state: 'ready', message: '', signInUrl: null, signInLabel: null };
}

/** True when the composer should accept input. `paused` and `quota` are refusals, not errors. */
export function canChat(status: AiStatus): boolean {
  return status.state === 'ready';
}

/**
 * Render an ISO reset timestamp as "00:00 UTC". The server promises UTC and the plan says so in
 * the message; converting to local time here would make the two disagree for anyone not in UTC.
 */
export function formatReset(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return 'at midnight UTC';
  const hours = String(at.getUTCHours()).padStart(2, '0');
  const minutes = String(at.getUTCMinutes()).padStart(2, '0');
  return `${hours}:${minutes} UTC on ${at.getUTCDate()} ${at.toLocaleString('en', { month: 'short', timeZone: 'UTC' })}`;
}

/**
 * A short line for the header: "Ada · 3/10 today" or nothing at all when signed out. Kept separate
 * from `aiStatus` because the header and the composer answer different questions.
 */
export function accountSummary(account: Account | null): string {
  if (!account) return 'not signed in';
  const name = account.displayName || 'signed in';
  if (!account.chatEnabled) return name;
  return `${name} · ${account.quota.used}/${account.quota.limit} today`;
}

/**
 * Fetch the server's public config. Never throws: a missing backend is a supported mode, and
 * turning it into an exception would make `npm run dev` log a scary error.
 */
export async function loadConfig(fetchImpl: typeof fetch = fetch): Promise<ServerConfig | null> {
  try {
    const response = await fetchImpl('/api/config', { headers: { Accept: 'application/json' } });
    if (!response.ok) return null;
    const body: unknown = await response.json();
    return isServerConfig(body) ? body : null;
  } catch {
    return null;
  }
}

/**
 * Fetch the signed-in user, or null when anonymous or when there is no backend.
 *
 * A 401 is the normal answer for a browser with no cookie, so it is not an error. A 5xx is a real
 * problem, but the composer's response is the same either way: it does not let the user spend
 * money. The reason is still worth distinguishing in the status object, which `loadAccount`
 * reports by returning `{ account: null, failed: true }`.
 */
export async function loadAccount(
  fetchImpl: typeof fetch = fetch,
): Promise<{ account: Account | null; failed: boolean }> {
  let response: Response;
  try {
    response = await fetchImpl('/api/me', { headers: { Accept: 'application/json' } });
  } catch {
    return { account: null, failed: true };
  }
  if (response.status === 401) return { account: null, failed: false };
  if (!response.ok) return { account: null, failed: true };
  try {
    const body: unknown = await response.json();
    return isAccount(body) ? { account: body, failed: false } : { account: null, failed: true };
  } catch {
    return { account: null, failed: true };
  }
}

/** POST /api/auth/logout. The caller reloads; this only reports whether it worked. */
export async function signOut(fetchImpl: typeof fetch = fetch): Promise<boolean> {
  try {
    const response = await fetchImpl('/api/auth/logout', {
      method: 'POST',
      headers: { Accept: 'application/json' },
    });
    return response.ok || response.status === 401;
  } catch {
    return false;
  }
}

// The server's JSON is untrusted input like any other: a field rename or a proxy's error page must
// produce a disabled composer, not a crash in the middle of rendering.

function isServerConfig(value: unknown): value is ServerConfig {
  if (typeof value !== 'object' || value === null) return false;
  const v = value as Record<string, unknown>;
  return (
    typeof v.chatEnabled === 'boolean' &&
    Array.isArray(v.providers) &&
    v.providers.every((p) => typeof p === 'string') &&
    typeof v.version === 'string'
  );
}

function isAccount(value: unknown): value is Account {
  if (typeof value !== 'object' || value === null) return false;
  const v = value as Record<string, unknown>;
  if (typeof v.id !== 'string' || typeof v.displayName !== 'string') return false;
  if (typeof v.chatEnabled !== 'boolean' || typeof v.inFlight !== 'boolean') return false;
  const quota = v.quota;
  if (typeof quota !== 'object' || quota === null) return false;
  const q = quota as Record<string, unknown>;
  return (
    typeof q.used === 'number' && typeof q.limit === 'number' && typeof q.resetsAt === 'string'
  );
}
