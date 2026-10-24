import { test } from 'node:test';
import assert from 'node:assert/strict';
import { Box, Circle, DistanceJoint, Edge, World } from 'planck';
import { buildFrame } from './frame.js';

const DT = 1 / 60;

function fixture() {
  const world = new World({ gravity: { x: 0, y: 0 } });

  const ground = world.createBody({ type: 'static', position: { x: 0, y: 0 }, angle: Math.PI / 2 });
  ground.setUserData('ground');
  ground.createFixture({ shape: new Edge({ x: -2, y: 0 }, { x: 2, y: 0 }) });

  const ball = world.createBody({ type: 'dynamic', position: { x: 3, y: 4 } });
  ball.setUserData('ball');
  ball.createFixture(new Circle(0.5), { density: 1 });

  return buildFrame([ground, ball], DT);
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

  const shape = buildFrame([box], DT).bodies[0]!.shapes[0]!;
  assert.equal(shape.kind, 'polyline');
  if (shape.kind !== 'polyline') return;
  assert.equal(shape.closed, true);
  assert.equal(shape.points.length, 4);
});

test('dynamic bodies report their weight as a gravity force', () => {
  const world = new World({ gravity: { x: 0, y: -10 } });
  const ball = world.createBody({ type: 'dynamic', position: { x: 0, y: 0 } });
  ball.createFixture(new Circle(0.5), { density: 1 });

  world.step(DT);
  const [gravity] = buildFrame([ball], DT).bodies[0]!.forces;

  assert.equal(gravity?.kind, 'gravity');
  assert.ok(gravity);
  assert.deepEqual(g(gravity.force), [0, round(-10 * ball.getMass())]);
  // Gravity acts at the centre of mass, which has already moved a hair by the step.
  assert.deepEqual(g(gravity.at), g(ball.getWorldCenter()));
});

test('static bodies report no forces', () => {
  const world = new World({ gravity: { x: 0, y: -10 } });
  const ground = world.createBody({ type: 'static' });
  ground.createFixture(new Edge({ x: -5, y: -5 }, { x: 5, y: -5 }));

  world.step(DT);
  assert.deepEqual(buildFrame([ground], DT).bodies[0]!.forces, []);
});

test('a hanging ball balances its weight against the joint reaction', () => {
  const world = new World({ gravity: { x: 0, y: -10 } });
  const anchor = world.createBody({ type: 'static', position: { x: 0, y: 0 } });
  const ball = world.createBody({ type: 'dynamic', position: { x: 0, y: -2 } });
  ball.createFixture(new Circle(0.4), { density: 1 });
  world.createJoint(new DistanceJoint({}, anchor, ball, { x: 0, y: 0 }, { x: 0, y: -2 }));

  for (let i = 0; i < 120; i++) world.step(DT);
  const forces = buildFrame([anchor, ball], DT).bodies[1]!.forces;

  const gravity = forces.find((f) => f.kind === 'gravity')!;
  const joint = forces.find((f) => f.kind === 'joint')!;
  assert.ok(gravity && joint, 'expected a gravity and a joint force');
  // The joint pulls up nearly as hard as gravity pulls down, so the ball hangs.
  assert.ok(Math.abs(joint.force.y + gravity.force.y) < Math.abs(gravity.force.y) * 0.05);
  assert.deepEqual(g(joint.at), [0, -2]);
});

test('joint force points down on the anchor body, up on the hanging body', () => {
  const world = new World({ gravity: { x: 0, y: -10 } });
  const anchor = world.createBody({ type: 'static', position: { x: 0, y: 0 } });
  anchor.setUserData('anchor');
  const ball = world.createBody({ type: 'dynamic', position: { x: 0, y: -2 } });
  ball.setUserData('ball');
  ball.createFixture(new Circle(0.4), { density: 1 });
  world.createJoint(new DistanceJoint({}, anchor, ball, { x: 0, y: 0 }, { x: 0, y: -2 }));

  for (let i = 0; i < 120; i++) world.step(DT);
  const ballJoint = buildFrame([anchor, ball], DT).bodies[1]!.forces.find((f) => f.kind === 'joint')!;

  // Newton's third law: the ball is pulled toward the anchor (+y).
  assert.ok(ballJoint.force.y > 0, `expected upward pull on the ball, got ${ballJoint.force.y}`);
});

test('resting ball gets an upward contact force equal to its weight', () => {
  const world = new World({ gravity: { x: 0, y: -10 } });
  const ground = world.createBody({ type: 'static', position: { x: 0, y: 0 } });
  ground.createFixture(new Edge({ x: -5, y: 0 }, { x: 5, y: 0 }));
  const ball = world.createBody({ type: 'dynamic', position: { x: 0, y: 1.2 } });
  ball.createFixture(new Circle(0.5), { density: 1 });

  for (let i = 0; i < 240; i++) world.step(DT);
  const forces = buildFrame([ground, ball], DT).bodies[1]!.forces;

  const contact = forces.filter((f) => f.kind === 'contact');
  assert.ok(contact.length > 0, 'expected at least one contact force once the ball lands');
  const totalY = contact.reduce((sum, f) => sum + f.force.y, 0);
  assert.ok(Math.abs(totalY - ball.getMass() * 10) < ball.getMass() * 0.5, `contact ${totalY}`);
  // Contacts act on the ball's underside, not its centre.
  for (const f of contact) assert.ok(f.at.y < 0.6, `contact anchor y=${f.at.y}`);
});

test('a joint is reported once, between its two world anchor points', () => {
  // Both bodies static: buildFrame reads joint reactions only for dynamic ones,
  // and an unstepped joint has none to report.
  const world = new World({ gravity: { x: 0, y: 0 } });
  const anchor = world.createBody({ type: 'static', position: { x: 0, y: 0 } });
  const other = world.createBody({ type: 'static', position: { x: 3, y: 4 } });
  // Constructor anchors are world-space: (0,0) on body A, (3,4) — body B's centre — on B.
  world.createJoint(new DistanceJoint({}, anchor, other, { x: 0, y: 0 }, { x: 3, y: 4 }));

  const { joints } = buildFrame([anchor, other], DT);
  assert.equal(joints.length, 1, 'joint seen from both bodies must appear once');
  assert.deepEqual(g(joints[0]!.a), [0, 0]);
  assert.deepEqual(g(joints[0]!.b), [3, 4]);
});

test('a body with no joints reports none', () => {
  assert.deepEqual(fixture().joints, []);
});

test('energy is kinetic plus potential, summed over dynamic bodies only', () => {
  const world = new World({ gravity: { x: 0, y: -10 } });
  const ground = world.createBody({ type: 'static', position: { x: 0, y: 0 } });
  ground.createFixture(new Edge({ x: -5, y: 0 }, { x: 5, y: 0 }));

  const ball = world.createBody({ type: 'dynamic', position: { x: 0, y: 4 } });
  ball.createFixture(new Circle(0.5), { density: 1 });
  ball.setLinearVelocity({ x: 3, y: 0 });

  // Unstepped: velocity and position are exactly what was set above.
  const { energy } = buildFrame([ground, ball], DT);
  const mass = ball.getMass();

  assert.equal(round(energy.kinetic), round(0.5 * mass * 9));
  // U = -m*g*r: gravity pulls toward -y, the ball sits at +4 m.
  assert.equal(round(energy.potential), round(mass * 10 * 4));
});

test('a falling ball trades potential for kinetic, keeping the total', () => {
  const world = new World({ gravity: { x: 0, y: -10 } });
  const ball = world.createBody({ type: 'dynamic', position: { x: 0, y: 10 } });
  ball.createFixture(new Circle(0.5), { density: 1 });

  const before = buildFrame([ball], DT).energy;
  for (let i = 0; i < 30; i++) world.step(DT);
  const after = buildFrame([ball], DT).energy;

  assert.ok(after.potential < before.potential, 'potential should drop as the ball falls');
  assert.ok(after.kinetic > before.kinetic, 'kinetic should rise as the ball falls');
  const beforeTotal = before.kinetic + before.potential;
  const afterTotal = after.kinetic + after.potential;
  assert.ok(Math.abs(afterTotal - beforeTotal) < Math.abs(beforeTotal) * 0.02 + 0.1, `total drifted ${beforeTotal} -> ${afterTotal}`);
});

/** Rounded [x, y] so float noise does not defeat deepEqual. */
function g(v: { x: number; y: number }): [number, number] {
  return [round(v.x), round(v.y)];
}

function fail(message: string): never {
  throw new Error(message);
}

function round(n: number): number {
  // `|| 0` normalises -0 so deepEqual does not trip on it
  return Math.round(n * 1e6) / 1e6 || 0;
}
