import { test } from 'node:test';
import assert from 'node:assert/strict';
import { worldToScreen } from './camera.js';

const camera = { center: { x: 0, y: 0 }, scale: 10 };
const viewport = { width: 200, height: 100 };

test('world centre maps to viewport centre', () => {
  assert.deepEqual(worldToScreen(camera, viewport, { x: 0, y: 0 }), { x: 100, y: 50 });
});

test('world +x goes right and +y goes up the screen', () => {
  assert.deepEqual(worldToScreen(camera, viewport, { x: 2, y: 0 }), { x: 120, y: 50 });
  assert.deepEqual(worldToScreen(camera, viewport, { x: 0, y: 2 }), { x: 100, y: 30 });
});

test('camera centre pans the view', () => {
  const panned = { ...camera, center: { x: 2, y: 0 } };
  assert.deepEqual(worldToScreen(panned, viewport, { x: 2, y: 0 }), { x: 100, y: 50 });
});
