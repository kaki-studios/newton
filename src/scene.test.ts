import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createScene } from './scene.js';

test('scene has a static platform and named bodies', () => {
  const { world, bodies } = createScene();
  assert.equal(world.getBodyCount(), 2);
  assert.deepEqual(
    bodies.map((b) => [b.getUserData(), b.getType()]),
    [['platform', 'static'], ['ball', 'dynamic']],
  );
});

test('dynamic bodies fall under gravity', () => {
  const { world, bodies } = createScene();
  const ball = bodies[1]!;

  const startY = ball.getPosition().y;
  for (let i = 0; i < 60; i++) world.step(1 / 60);
  const midY = ball.getPosition().y;
  for (let i = 0; i < 60; i++) world.step(1 / 60);

  assert.ok(midY < startY, `expected fall in first second, got y=${midY}`);
  assert.ok(ball.getPosition().y < midY, 'velocity never picked up');
});
