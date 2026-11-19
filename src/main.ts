import { buildSceneJson, type Scene } from './scene/index.js';
import { buildFrame } from './render/frame.js';
import { CanvasRenderer } from './render/canvas2d.js';
import type { Camera, Viewport } from './render/types.js';
import { createAccumulator } from './loop.js';
import { Chat, type ChatMessage, type Verdict } from './chat.js';
import { aiBackend, deliver, judge } from './ai/glue.js';
import { ChatError, streamChat } from './ai/client.js';
import { fakeStream, PENDULUM } from './ai/fake.js';
import { createTabs } from './tabs.js';
import {
  accountSummary,
  afterChatError,
  aiStatus,
  canChat,
  loadAccount,
  loadConfig,
  signOut,
  type Account,
  type AiStatus,
  type ServerConfig,
} from './account.js';

/** The document the editor starts from. */
const SCENE_URL = '/dist/scene.json';

/** Rendering state, separate from the scene. */
const camera: Camera = { center: { x: 0, y: -5 }, scale: 24 };

const canvas = required('canvas', HTMLCanvasElement);
const panel = required('#panel', HTMLDetailsElement);
const textArea = required('#scene-text', HTMLTextAreaElement);
const applyButton = required('#apply', HTMLButtonElement);
const sceneStatus = required('#scene-status', HTMLElement);
const tabChat = required('#tab-chat', HTMLButtonElement);
const tabJson = required('#tab-json', HTMLButtonElement);
const chatPanel = required('#chat-panel', HTMLElement);
const jsonPanel = required('#scene-json', HTMLElement);
const chatLog = required('#chat-log', HTMLOListElement);
const chatForm = required('#chat-form', HTMLFormElement);
const chatInput = required('#chat-input', HTMLTextAreaElement);
const chatSend = required('#chat-send', HTMLButtonElement);
const chatStatus = required('#chat-status', HTMLElement);
const accountName = required('#account-name', HTMLElement);
const accountSignIn = required('#account-signin', HTMLAnchorElement);
const accountSignOut = required('#account-signout', HTMLButtonElement);
const aiStatusLine = required('#ai-status', HTMLElement);
const chatNew = required('#chat-new', HTMLButtonElement);

/**
 * `?ai=fake` swaps the server for a canned stream, so the whole loop — stream, extract, validate,
 * apply — can be watched under `npm run dev` with no backend. Without it, static mode is exactly
 * the editor it has always been.
 */
const fakeMode = new URLSearchParams(location.search).get('ai') === 'fake';
const FAKE_STATUS: AiStatus = {
  state: 'ready',
  message: 'Fake AI (?ai=fake): canned replies, nothing leaves the browser',
  signInUrl: null,
  signInLabel: null,
};

const tabs = createTabs([
  { tab: tabChat, panel: chatPanel },
  { tab: tabJson, panel: jsonPanel },
]);

/** Physics timestep. Simulation speed is independent of this: see the loop below. */
const DT = 1 / 120;
const stepsDue = createAccumulator(DT);

/** `null` until the document in the textarea builds: the loop idles until then. */
let scene: Scene | null = null;
/** Recreated on every apply: render state like the energy chart belongs to one scene. */
let renderer = new CanvasRenderer(canvas);
let last = performance.now();

/** Canvas CSS box, not the window: the editor panel shares the window with it. */
function viewport(): Viewport {
  return { width: canvas.clientWidth, height: canvas.clientHeight };
}

window.addEventListener('resize', () => renderer.resize(viewport()));
// Collapsing the panel gives the canvas more room, and that fires no `resize` event.
panel.addEventListener('toggle', () => renderer.resize(viewport()));

/** Parse the textarea into a running scene, or report why it cannot be one. */
function apply(): void {
  try {
    scene = buildSceneJson(textArea.value);
  } catch (error) {
    // SceneError already lists every problem with its path; the old scene keeps running.
    report(sceneStatus, error);
    return;
  }

  sceneStatus.textContent = '';
  sceneStatus.classList.remove('error');
  // A fresh renderer drops the old scene's energy history, and resetting the clock stops
  // the time spent editing from being replayed as a burst of physics on the next frame.
  renderer = new CanvasRenderer(canvas);
  renderer.resize(viewport());
  last = performance.now();
}

applyButton.addEventListener('click', apply);

/**
 * The chat log is the DOM; `Chat` only holds messages, so render them on change.
 *
 * A finished reply is judged by the same `buildSceneJson` the Apply button runs. A valid one goes
 * into the textarea and is applied, so the user can see and edit exactly what runs; an invalid one
 * leaves both alone and the old scene keeps playing.
 */
const backend = fakeMode
  ? fakeStream(PENDULUM, { chunk: 6, delayMs: 12 })
  : aiBackend({ stream: streamChat, getScene: () => textArea.value, onDone: () => void refreshAccount() });
const chat = new Chat(backend, {
  judge(text) {
    const verdict = judge(text);
    deliver(verdict, {
      setText: (document) => {
        textArea.value = document;
      },
      apply,
    });
    return verdict;
  },
});

/** One DOM row per message. Rows are kept rather than rebuilt so a streaming reply is a single
 *  text mutation per delta, and `aria-live` does not re-announce every turn on each update. */
interface Row {
  li: HTMLLIElement;
  text: HTMLSpanElement;
  verdict: HTMLElement | null;
  fix: HTMLButtonElement | null;
}
const rows: Row[] = [];
/** Set on submit: the disabled input drops focus, so the reply brings the cursor back. */
let focusComposer = false;

function renderChat(): void {
  const history = chat.history();
  // The log only shrinks on reset (or when a whitespace-only reply is dropped): start over.
  if (history.length < rows.length) {
    chatLog.replaceChildren();
    rows.length = 0;
  }

  history.forEach((message, i) => {
    let row = rows[i];
    if (!row) {
      row = messageRow(message);
      rows.push(row);
      chatLog.append(row.li);
    }
    if (row.text.textContent !== message.text) row.text.textContent = message.text;
    if (message.status) row.li.dataset.status = message.status;
    else delete row.li.dataset.status;
    if (message.verdict && !row.verdict) {
      Object.assign(row, verdictView(message.verdict));
      row.li.append(row.verdict!);
    }
  });

  // Only the newest reply can be repaired: an older one has already been superseded.
  rows.forEach((row, i) => {
    if (row.fix) row.fix.hidden = !(i === rows.length - 1 && chat.canRepair && canChat(ai));
  });

  renderComposer();
  chatLog.scrollTop = chatLog.scrollHeight; // the newest turn is what you want to read

  if (!chat.isPending && focusComposer) {
    focusComposer = false;
    // Switching tabs while a reply is pending must not drag focus off the visible panel.
    if (tabs.selected === tabChat.id) chatInput.focus();
  }
}

/**
 * The composer is enabled only when the AI is usable *and* no reply is in flight. Both callers —
 * a chat change and an account change — go through here, so the two conditions cannot drift apart
 * and a finished turn cannot re-enable a composer that is off because nobody is signed in.
 *
 * While a reply streams, the send button is Stop: that is the only thing worth clicking then.
 */
function renderComposer(): void {
  const pending = chat.isPending;
  const usable = canChat(ai);
  chatInput.disabled = !usable || pending;
  chatSend.disabled = !usable && !pending;
  chatSend.textContent = pending ? 'Stop' : 'Send';
  chatInput.setAttribute('aria-disabled', String(chatInput.disabled));
  chatSend.setAttribute('aria-disabled', String(chatSend.disabled));
  chatNew.hidden = rows.length === 0;
  chatNew.disabled = pending;
  chatLog.setAttribute('aria-busy', String(pending));
}

function messageRow(message: ChatMessage): Row {
  const li = document.createElement('li');
  li.dataset.role = message.role;
  const role = document.createElement('span');
  role.className = 'role';
  role.textContent = message.role;
  const text = document.createElement('span');
  text.className = 'text';
  text.textContent = message.text; // textContent, never innerHTML: the reply is untrusted
  li.append(role, text);
  return { li, text, verdict: null, fix: null };
}

/** "✓ valid — applied", or every issue path and an Ask to fix button. */
function verdictView(verdict: Verdict): { verdict: HTMLElement; fix: HTMLButtonElement | null } {
  const box = document.createElement('div');
  box.className = 'verdict';
  box.dataset.ok = String(verdict.ok);
  const summary = document.createElement('span');
  box.append(summary);

  if (verdict.ok) {
    summary.textContent = '✓ valid — applied';
    return { verdict: box, fix: null };
  }

  const count = verdict.issues.length;
  summary.textContent = `✗ ${count} ${count === 1 ? 'issue' : 'issues'} — the running scene is unchanged`;
  const list = document.createElement('ul');
  for (const issue of verdict.issues) {
    const item = document.createElement('li');
    item.textContent = issue.path ? `${issue.path}: ${issue.message}` : issue.message;
    list.append(item);
  }
  const fix = document.createElement('button');
  fix.type = 'button';
  fix.className = 'fix';
  fix.textContent = 'Ask to fix';
  fix.addEventListener('click', () => {
    focusComposer = true;
    void requestReply(() => chat.repair());
  });
  box.append(list, fix);
  return { verdict: box, fix };
}

chat.subscribe(renderChat);

/**
 * Who is signed in and whether the AI can be used. Both are refreshed on load, after a reply (the
 * quota counter moved) and after a 429; there is no polling.
 */
let serverConfig: ServerConfig | null = null;
let account: Account | null = null;
let ai: AiStatus = fakeMode ? FAKE_STATUS : aiStatus(null, null);

function renderAccount(): void {
  accountName.textContent = account
    ? accountSummary(account)
    : serverConfig
      ? 'not signed in'
      : 'no backend';

  // The link and the button are mutually exclusive: showing both invites the wrong click.
  if (ai.signInUrl && ai.signInLabel) {
    accountSignIn.href = ai.signInUrl;
    accountSignIn.textContent = ai.signInLabel;
    accountSignIn.hidden = false;
  } else {
    accountSignIn.hidden = true;
  }
  accountSignOut.hidden = account === null;

  aiStatusLine.textContent = ai.message;
  aiStatusLine.dataset.state = ai.state;
  chatInput.dataset.state = ai.state;
  renderChat(); // the fix buttons depend on `ai` too
}

/** Re-read `/api/me`. A failed read keeps what is on screen: it is a refresh, not a sign-out. */
async function refreshAccount(): Promise<void> {
  if (!serverConfig) return;
  const loaded = await loadAccount();
  if (loaded.failed) return;
  account = loaded.account;
  if (!fakeMode) ai = aiStatus(serverConfig, account);
  renderAccount();
}

accountSignOut.addEventListener('click', async () => {
  // Sign out, then reload: the scene keeps running and the server has already revoked the cookie,
  // so a reload is the simplest way to get every derived bit of state back in sync.
  if (await signOut()) location.reload();
  else report(chatStatus, new Error('signing out failed: the server did not revoke the session'));
});

/** A new conversation is also the way past the per-conversation turn cap. */
chatNew.addEventListener('click', () => {
  if (chat.isPending) return;
  chat.reset();
  clear(chatStatus);
  if (canChat(ai)) chatInput.focus();
});

chatForm.addEventListener('submit', (event) => {
  event.preventDefault(); // a submit would reload the page and drop the running scene
  if (chat.isPending) {
    chat.cancel(); // the button reads Stop while a reply streams
    return;
  }
  const text = chatInput.value;
  if (!text.trim()) return;
  chatInput.value = '';
  focusComposer = true;
  void requestReply(() => chat.send(text));
});

/** Enter sends, Shift+Enter adds a line — the hint in index.html promises exactly this. */
chatInput.addEventListener('keydown', (event) => {
  if (event.key !== 'Enter' || event.shiftKey || event.isComposing) return;
  event.preventDefault();
  chatForm.requestSubmit();
});

async function requestReply(turn: () => Promise<void>): Promise<void> {
  clear(chatStatus);
  try {
    await turn();
  } catch (error) {
    // `Chat` keeps the user turn (and any partial reply), so the text is still on screen next to
    // the reason.
    if (error instanceof ChatError) {
      if (!fakeMode) ai = afterChatError(ai, error, serverConfig);
      renderAccount();
      // 429 is the daily quota *or* this conversation's turn cap; /api/me tells them apart.
      if (error.code === 'quota_exceeded') void refreshAccount();
      if (error.code === 'canceled') {
        chatStatus.textContent = fakeMode
          ? 'stopped'
          : 'stopped — a cancelled reply still counts against today\'s quota';
        return;
      }
    }
    report(chatStatus, error);
  }
}

try {
  textArea.value = await fetchText(SCENE_URL);
  apply();
} catch (error) {
  // The default document is unavailable, but the editor still works: show why and go on.
  report(sceneStatus, error);
}
renderer.resize(viewport());

// The AI is additive: a server that does not answer /api/config is `npm run dev` with a static
// file server, which is a supported mode rather than an error path.
serverConfig = await loadConfig();
if (serverConfig) {
  const loaded = await loadAccount();
  account = loaded.account;
  if (loaded.failed) {
    report(chatStatus, new Error('could not read your account; the AI is off until the page reloads'));
  }
}
if (!fakeMode) ai = aiStatus(serverConfig, account);
renderAccount(); // also the chat's initial paint

/** One physics step per frame would make sim speed 60·DT; step by elapsed wall time instead. */
function frame(now: number): void {
  requestAnimationFrame(frame);
  if (!scene) return;

  const steps = stepsDue((now - last) / 1000);
  last = now;
  if (steps === 0) return; // drawing costs a redraw of every shape, and the screen is unchanged

  for (let i = 0; i < steps; i++) scene.world.step(DT);
  renderer.draw(buildFrame(scene.bodies, DT), camera);
}

requestAnimationFrame(frame);

function report(target: HTMLElement, error: unknown): void {
  target.textContent = error instanceof Error ? error.message : String(error);
  target.classList.add('error');
}

function clear(target: HTMLElement): void {
  target.textContent = '';
  target.classList.remove('error');
}

/** Fetch a scene document as text: the editor shows exactly what gets parsed. */
async function fetchText(url: string): Promise<string> {
  let response: Response;
  try {
    response = await fetch(url);
  } catch (error) {
    throw new Error(`could not fetch ${url}: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (!response.ok) {
    throw new Error(`could not load ${url}: ${response.status} ${response.statusText}`);
  }
  return response.text();
}

/** The DOM contract lives in index.html; a typo there should fail loudly, not silently no-op. */
function required<T extends Element>(selector: string, type: abstract new () => T): T {
  const element = document.querySelector(selector);
  if (!(element instanceof type)) {
    throw new Error(`index.html is missing ${selector}: the editor cannot work without it`);
  }
  return element;
}
