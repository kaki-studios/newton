import { buildSceneJson, type Scene } from './scene/index.js';
import { buildFrame } from './render/frame.js';
import { CanvasRenderer } from './render/canvas2d.js';
import type { Camera, Viewport } from './render/types.js';
import { createAccumulator } from './loop.js';

/** The document the editor starts from. */
const SCENE_URL = '/dist/scene.json';

const camera: Camera = { center: { x: 0, y: -5 }, scale: 24 };

const canvas = required('canvas', HTMLCanvasElement);
const panel = required('#panel', HTMLDetailsElement);
const textArea = required('#scene-text', HTMLTextAreaElement);
const applyButton = required('#apply', HTMLButtonElement);
const status = required('#status', HTMLElement);

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
    report(error);
    return;
  }

  status.textContent = '';
  status.classList.remove('error');
  // A fresh renderer drops the old scene's energy history, and resetting the clock stops
  // the time spent editing from being replayed as a burst of physics on the next frame.
  renderer = new CanvasRenderer(canvas);
  renderer.resize(viewport());
  last = performance.now();
}

function report(error: unknown): void {
  status.textContent = error instanceof Error ? error.message : String(error);
  status.classList.add('error');
}

applyButton.addEventListener('click', apply);

try {
  textArea.value = await fetchText(SCENE_URL);
  apply();
} catch (error) {
  // The default document is unavailable, but the editor still works: show why and go on.
  report(error);
}
renderer.resize(viewport());

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
