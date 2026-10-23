import { createScene } from './scene.js';
import { buildFrame } from './render/frame.js';
import { CanvasRenderer } from './render/canvas2d.js';
import type { Camera, Viewport } from './render/types.js';

const camera: Camera = { center: { x: 0, y: -5 }, scale: 24 };
const canvas = document.querySelector('canvas');
if (!(canvas instanceof HTMLCanvasElement)) {
  throw new Error('no <canvas> in the page: add <canvas></canvas> to index.html');
}

const renderer = new CanvasRenderer(canvas);
const { world, bodies } = createScene();

function viewport(): Viewport {
  return { width: window.innerWidth, height: window.innerHeight };
}

function resize(): void {
  renderer.resize(viewport());
}

window.addEventListener('resize', resize);
resize();

function frame(): void {
  world.step(1 / 60);
  renderer.draw(buildFrame(bodies), camera);
  requestAnimationFrame(frame);
}

requestAnimationFrame(frame);
