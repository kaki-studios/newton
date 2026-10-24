import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { buildFrame } from './render/frame.js';
import { buildScene } from './scene/index.js';

const DT = 1 / 60;

/**
 * Guards `src/demo.json` — the document `main.ts` fetches, and the example of the format.
 * Without this, a typo in the JSON only shows up as a blank canvas in the browser.
 */
function scene() {
  // dist/demo.json sits next to this compiled test, mirroring src/.
  const url = new URL('./demo.json', import.meta.url);
  try {
    return buildScene(JSON.parse(readFileSync(fileURLToPath(url), 'utf8')));
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    assert.fail(`src/demo.json is not a valid scene: ${message}`);
  }
}

test('src/demo.json parses and builds', () => {
  const { world, bodies } = scene();

  assert.deepEqual(
    bodies.map((b) => [b.getUserData(), b.getType()]),
    [['platform', 'static'], ['ball', 'dynamic']],
  );
  assert.equal(world.getJointCount(), 1);
});

test('the demo scene starts at rest and stays there: the rod carries the weight', () => {
  const { world, bodies } = scene();
  for (let i = 0; i < 120; i++) world.step(DT);

  const ball = bodies[1]!;
  assert.ok(ball.getLinearVelocity().length() < 0.01, `ball should hang still, velocity ${ball.getLinearVelocity().length()}`);
  // The anchor is 2 m above the platform, so the ball hangs 2 m below it.
  assert.ok(Math.abs(ball.getPosition().y - 4) < 0.01, `ball drifted to y=${ball.getPosition().y}`);
});

test('the demo scene renders through the existing Frame contract', () => {
  const { world, bodies } = scene();
  world.step(DT);
  const frame = buildFrame(bodies, DT);

  assert.deepEqual(frame.bodies.map((b) => b.id), ['platform', 'ball']);
  assert.equal(frame.joints.length, 1);
  assert.ok(frame.bodies[1]!.forces.some((f) => f.kind === 'gravity'));
  assert.ok(frame.bodies[1]!.forces.some((f) => f.kind === 'joint'));
});
