import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { buildSceneJson, SceneError } from './index.js';

/** `dist/scene.json` sits one level up from this compiled test, mirroring src/. */
function sceneJson(): string {
  return readFileSync(fileURLToPath(new URL('../scene.json', import.meta.url)), 'utf8');
}

test('src/scene.json — the editor default — builds and simulates', () => {
  const { world, bodies } = buildSceneJson(sceneJson());

  assert.deepEqual(bodies.map((b) => b.getUserData()), ['platform', 'upper', 'lower']);
  for (let i = 0; i < 60; i++) world.step(1 / 60);

  // The platform sits at y=-10 and its anchor is local (0, 20), so the rod hangs from
  // (0, 10) with length 6: the ball swings on it instead of falling out of the world.
  const p = bodies[1]!.getPosition();
  assert.ok(Math.abs(Math.hypot(p.x, p.y - 10) - 6) < 0.05, `upper left the rod: at (${p.x}, ${p.y})`);
  assert.ok(p.y > -10, `upper fell through the platform: y=${p.y}`);
});

test('broken JSON in the editor reports a SceneError, not a SyntaxError', () => {
  assert.throws(
    () => buildSceneJson('{"bodies": [}'),
    (error: unknown) => error instanceof SceneError && error.kind === 'syntax' && /not valid JSON/.test(error.message),
  );
});

test('a semantic problem in the editor keeps its path in the message', () => {
  const doc = JSON.stringify({
    bodies: [{ id: 'a', type: 'dynamic', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 1 } }],
    joints: [{ kind: 'distance', bodyA: 'a', bodyB: 'a' }],
  });

  assert.throws(
    () => buildSceneJson(doc),
    (error: unknown) => error instanceof SceneError && /joints\[0\]/.test(error.message),
  );
});
