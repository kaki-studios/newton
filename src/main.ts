import { buildSceneJson, type Scene } from './scene/index.js';
import { buildFrame } from './render/frame.js';
import { CanvasRenderer } from './render/canvas2d.js';
import type { Camera, Viewport } from './render/types.js';
import { createAccumulator } from './loop.js';
import { Chat, dummyChat, type ChatMessage } from './chat.js';
import { createTabs } from './tabs.js';
import {
  accountSummary,
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
const chatHint = required('#chat-hint', HTMLElement);

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
 * `dummyChat` is still the backend: the streaming client (`src/ai/*`) is the next milestone, and the
 * seam it plugs into is exactly this argument. Until then the composer is honest about it — see the
 * hint below — because a chat box that answers "dummy" with no explanation reads as a bug.
 */
const chat = new Chat(dummyChat);
/** Rows already in the DOM. The conversation only grows, so only the new tail is added,
 *  which keeps `aria-live` from re-announcing every turn on each update. */
let rendered = 0;
/** Set on submit: the disabled input drops focus, so the reply brings the cursor back. */
let focusComposer = false;

function renderChat(): void {
  const history = chat.history();
  chatLog.append(...history.slice(rendered).map(messageRow));
  rendered = history.length;
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
 */
function renderComposer(): void {
  const usable = canChat(ai) && !chat.isPending;
  chatSend.disabled = !usable;
  chatInput.disabled = !usable;
  for (const element of [chatInput, chatSend]) {
    element.setAttribute('aria-disabled', String(!usable));
  }
  chatLog.setAttribute('aria-busy', String(chat.isPending));
}

function messageRow(message: ChatMessage): HTMLLIElement {
  const row = document.createElement('li');
  row.dataset.role = message.role;
  const role = document.createElement('span');
  role.className = 'role';
  role.textContent = message.role;
  const text = document.createElement('span');
  text.textContent = message.text; // textContent, never innerHTML: the reply is untrusted
  row.append(role, text);
  return row;
}

chat.subscribe(renderChat);

/**
 * Who is signed in and whether the AI can be used. Both are refreshed on load and after sign-out;
 * there is no polling, because nothing here changes while the page is open except the quota, which
 * the next send re-reads anyway.
 */
let serverConfig: ServerConfig | null = null;
let account: Account | null = null;
let ai: AiStatus = aiStatus(null, null);

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
  // The backend streams today, but this bundle still answers locally. Saying so is better than a
  // reply of "dummy" that looks like a crash; the next milestone replaces this line with the stream.
  chatHint.textContent =
    ai.state === 'ready'
      ? 'Enter sends, Shift+Enter adds a line · model wiring arrives with src/ai/client.ts'
      : 'Enter sends, Shift+Enter adds a line';
  chatInput.dataset.state = ai.state;
  renderComposer();
}

accountSignOut.addEventListener('click', async () => {
  // Sign out, then reload: the scene keeps running and the server has already revoked the cookie,
  // so a reload is the simplest way to get every derived bit of state back in sync.
  if (await signOut()) location.reload();
  else report(chatStatus, new Error('signing out failed: the server did not revoke the session'));
});

chatForm.addEventListener('submit', (event) => {
  event.preventDefault(); // a submit would reload the page and drop the running scene
  const text = chatInput.value;
  if (!text.trim() || chat.isPending) return;
  chatInput.value = '';
  focusComposer = true;
  void requestReply(text);
});

/** Enter sends, Shift+Enter adds a line — the hint in index.html promises exactly this. */
chatInput.addEventListener('keydown', (event) => {
  if (event.key !== 'Enter' || event.shiftKey || event.isComposing) return;
  event.preventDefault();
  chatForm.requestSubmit();
});

async function requestReply(text: string): Promise<void> {
  try {
    await chat.send(text);
  } catch (error) {
    // `Chat` keeps the user turn, so the text is still on screen next to the reason.
    report(chatStatus, error);
    return;
  }
  chatStatus.textContent = '';
  chatStatus.classList.remove('error');
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
ai = aiStatus(serverConfig, account);
renderAccount();
renderChat(); // the initial paint; renderAccount only touches the account bar and the status line

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
