import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Box, Circle, Edge, World } from 'planck';
import { buildFrame } from './frame.js';

function fixture() {
  const world = new World({ gravity: { x: 0, y: 0 } });

  const ground = world.createBody({ type: 'static', position: { x: 0, y: 0 }, angle: Math.PI / 2 });
  ground.setUserData('ground');
  ground.createFixture({ shape: new Edge({ x: -2, y: 0 }, { x: 2, y: 0 }) });

  const ball = world.createBody({ type: 'dynamic', position: { x: 3, y: 4 } });
  ball.setUserData('ball');
  ball.createFixture(new Circle(0.5), { density: 1 });

  return buildFrame([ground, ball]);
}

test('edges render as open polylines, transformed into world space', () => {
  const [ground = fail('no ground body in frame')] = fixture().bodies;

  assert.equal(ground.id, 'ground');
  assert.equal(ground.isStatic, true);
  assert.equal(ground.shapes.length, 1);

  const shape = ground.shapes[0]!;
  assert.equal(shape.kind, 'polyline');
  if (shape.kind !== 'polyline') return;
  assert.equal(shape.closed, false);
  // edge rotated 90°: (-2,0)->(2,0) becomes (0,-2)->(0,2)
  assert.deepEqual(shape.points.map((p) => [round(p.x), round(p.y)]), [[0, -2], [0, 2]]);
});

test('circles keep centre and radius instead of being polygonised', () => {
  const ball = fixture().bodies[1]!;
  assert.equal(ball.id, 'ball');
  assert.equal(ball.isStatic, false);
  assert.deepEqual(ball.shapes[0], { kind: 'circle', center: { x: 3, y: 4 }, radius: 0.5 });
});

test('polygons render as closed polylines', () => {
  const world = new World({ gravity: { x: 0, y: 0 } });
  const box = world.createBody({ type: 'dynamic' });
  box.createFixture(new Box(1, 0.5));

  const shape = buildFrame([box]).bodies[0]!.shapes[0]!;
  assert.equal(shape.kind, 'polyline');
  if (shape.kind !== 'polyline') return;
  assert.equal(shape.closed, true);
  assert.equal(shape.points.length, 4);
});

function fail(message: string): never {
  throw new Error(message);
}

function round(n: number): number {
  // `|| 0` normalises -0 so deepEqual does not trip on it
  return Math.round(n * 1e6) / 1e6 || 0;
}
