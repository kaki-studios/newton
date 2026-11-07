import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  accountSummary,
  aiStatus,
  canChat,
  formatReset,
  loadAccount,
  loadConfig,
  signInUrl,
  signOut,
  type Account,
  type ServerConfig,
} from './account.js';

const config: ServerConfig = {
  chatEnabled: true,
  providers: ['github'],
  dev: false,
  version: 'test',
};

function account(overrides: Partial<Account> = {}): Account {
  return {
    id: 'u1',
    displayName: 'Ada',
    quota: { used: 2, limit: 10, resetsAt: '2026-03-11T00:00:00Z' },
    inFlight: false,
    chatEnabled: true,
    ...overrides,
  };
}

test('a signed-in user with quota left is ready to chat', () => {
  const status = aiStatus(config, account());
  assert.equal(status.state, 'ready');
  assert.equal(status.message, '');
  assert.equal(canChat(status), true);
});

test('an anonymous visitor is offered the configured provider by name', () => {
  const status = aiStatus(config, null);
  assert.equal(status.state, 'signed-out');
  assert.equal(status.message, 'Sign in with GitHub to use AI');
  assert.equal(status.signInUrl, '/api/auth/github');
  assert.equal(status.signInLabel, 'Sign in with GitHub');
  assert.equal(canChat(status), false);
});

test('a provider without a friendly label still produces a usable button', () => {
  const status = aiStatus({ ...config, providers: ['gitlab'] }, null);
  assert.equal(status.signInLabel, 'Sign in with gitlab');
});

test('the first configured provider is the one linked', () => {
  const status = aiStatus({ ...config, providers: ['github', 'google'] }, null);
  assert.equal(status.signInUrl, '/api/auth/github');
});

test('no backend is "unavailable in static mode", not "paused"', () => {
  // This is the `npm run dev` loop: python3 -m http.server answers 404 to /api/config.
  const status = aiStatus(null, null);
  assert.equal(status.state, 'unavailable');
  assert.match(status.message, /static mode/);
  assert.equal(status.signInUrl, null);
  assert.equal(canChat(status), false);
});

test('a server with no OAuth provider says so instead of offering a dead link', () => {
  const status = aiStatus({ ...config, providers: [] }, null);
  assert.equal(status.state, 'no-providers');
  assert.equal(status.signInUrl, null);
  assert.equal(canChat(status), false);
});

test('a spent quota reports both numbers and the reset time', () => {
  const status = aiStatus(
    config,
    account({ quota: { used: 10, limit: 10, resetsAt: '2026-03-11T00:00:00Z' } }),
  );
  assert.equal(status.state, 'quota');
  assert.match(status.message, /10\/10/);
  assert.match(status.message, /00:00 UTC/);
  assert.equal(canChat(status), false);
});

test('the kill switch wins over an available quota', () => {
  const status = aiStatus({ ...config, chatEnabled: false }, account({ chatEnabled: false }));
  assert.equal(status.state, 'paused');
  assert.equal(canChat(status), false);
});

test('a disabled account-side flag is enough to pause, even if config disagrees', () => {
  // The two sources can disagree for a moment during a deploy; refusing is the safe reading.
  assert.equal(aiStatus(config, account({ chatEnabled: false })).state, 'paused');
});

test('the public config wins when the two sources disagree', () => {
  // /api/config is the cacheable, public answer; a /api/me fetched a moment earlier must not
  // re-enable a composer the operator just switched off.
  const status = aiStatus({ ...config, chatEnabled: false }, account({ chatEnabled: true }));
  assert.equal(status.state, 'paused');
});

test('the reset time is formatted in UTC, not local time', () => {
  // 23:30 UTC on the 10th must not render as the 11th for anyone east of UTC.
  assert.equal(formatReset('2026-03-10T23:30:00Z'), '23:30 UTC on 10 Mar');
  // An unparseable value still produces a sentence rather than "Invalid Date".
  assert.equal(formatReset('garbage'), 'at midnight UTC');
});

test('the header summary names the user and the spend', () => {
  assert.equal(accountSummary(account()), 'Ada · 2/10 today');
  assert.equal(accountSummary(account({ chatEnabled: false })), 'Ada');
  assert.equal(accountSummary(null), 'not signed in');
  assert.equal(accountSummary(account({ displayName: '' })), 'signed in · 2/10 today');
});

test('signInUrl escapes the provider rather than pasting it in', () => {
  assert.equal(signInUrl('github'), '/api/auth/github');
  assert.equal(signInUrl('a/b'), '/api/auth/a%2Fb');
});

/** A fetch stand-in: node's fetch is not available in every environment the tests run in, and a
 *  stub makes the failure modes explicit. */
function stubFetch(answer: () => Promise<Response>): typeof fetch {
  return (async () => answer()) as unknown as typeof fetch;
}

test('loadConfig returns null instead of throwing when there is no backend', async () => {
  const offline = stubFetch(async () => {
    throw new TypeError('fetch failed');
  });
  assert.equal(await loadConfig(offline), null);

  const notFound = stubFetch(async () => new Response('no such API endpoint', { status: 404 }));
  assert.equal(await loadConfig(notFound), null);
});

test('loadConfig rejects a body that does not match the contract', async () => {
  const wrongShape = stubFetch(async () => Response.json({ providers: 'github' }));
  assert.equal(await loadConfig(wrongShape), null);

  const good = stubFetch(async () => Response.json(config));
  assert.deepEqual(await loadConfig(good), config);
});

test('a 401 from /api/me is anonymous, not a failure', async () => {
  const anonymous = stubFetch(async () => Response.json({ error: 'sign in' }, { status: 401 }));
  assert.deepEqual(await loadAccount(anonymous), { account: null, failed: false });
});

test('a 5xx from /api/me is a failure the caller can report', async () => {
  const broken = stubFetch(async () => new Response('{}', { status: 500 }));
  assert.deepEqual(await loadAccount(broken), { account: null, failed: true });
});

test('loadAccount validates the quota shape it renders', async () => {
  const missingQuota = stubFetch(async () => Response.json({ id: 'u1', displayName: 'Ada' }));
  assert.equal((await loadAccount(missingQuota)).failed, true);

  const good = stubFetch(async () => Response.json(account()));
  const loaded = await loadAccount(good);
  assert.equal(loaded.failed, false);
  assert.equal(loaded.account?.displayName, 'Ada');
});

test('loadAccount survives a body that is not JSON', async () => {
  const html = stubFetch(async () => new Response('<html>nginx</html>', { status: 200 }));
  assert.deepEqual(await loadAccount(html), { account: null, failed: true });
});

test('signOut reports success and treats a missing session as success', async () => {
  assert.equal(await signOut(stubFetch(async () => new Response(null, { status: 204 }))), true);
  assert.equal(await signOut(stubFetch(async () => new Response(null, { status: 401 }))), true);
  assert.equal(await signOut(stubFetch(async () => new Response(null, { status: 500 }))), false);
  assert.equal(
    await signOut(
      stubFetch(async () => {
        throw new TypeError('offline');
      }),
    ),
    false,
  );
});
