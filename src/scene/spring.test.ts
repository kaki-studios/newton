import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildFrame } from '../render/frame.js';
import { buildScene, SceneError } from './index.js';
import { parseScene } from './schema.js';
import { SpringJoint } from './spring.js';

const DT = 1 / 120;

/** Planck's circle mass: density · πr². */
const BALL_MASS = Math.PI * 0.5 * 0.5;

const anchor = { id: 'anchor', type: 'static', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.1 } };
const ballAt = (x: number, y: number) => ({ id: 'ball', type: 'dynamic', position: { x, y }, shape: { kind: 'circle', radius: 0.5 } });

test('a spring needs only bodies and stiffness; collisions stay on by default', () => {
  const spec = parseScene({ bodies: [anchor], joints: [{ kind: 'spring', bodyA: 'a', bodyB: 'b', stiffness: 5 }] });

  assert.deepEqual(spec.joints[0], {
    kind: 'spring',
    bodyA: 'a',
    bodyB: 'b',
    localAnchorA: { x: 0, y: 0 },
    localAnchorB: { x: 0, y: 0 },
    stiffness: 5,
    collideConnected: true,
  });
});

test('stiffness must be positive and restLength non-negative, each at its own path', () => {
  const error = catchSceneError(() =>
    parseScene({
      bodies: [anchor],
      joints: [
        { kind: 'spring', bodyA: 'a', bodyB: 'b', stiffness: 0 },
        { kind: 'spring', bodyA: 'a', bodyB: 'b', stiffness: 1, restLength: -1 },
        { kind: 'spring', bodyA: 'a', bodyB: 'b' },
        { kind: 'spring', bodyA: 'a', bodyB: 'b', stiffness: 1, length: 2 },
      ],
    }),
  );

  assert.deepEqual(error.issues.map((i) => i.path).sort(), ['joints[0].stiffness', 'joints[1].restLength', 'joints[2].stiffness', 'joints[3]'].sort());
});

test('a spring to a missing body is rejected like any joint', () => {
  const error = catchSceneError(() =>
    buildScene({ bodies: [anchor], joints: [{ kind: 'spring', bodyA: 'anchor', bodyB: 'ghost', stiffness: 1 }] }),
  );
  assert.deepEqual(error.issues.map((i) => i.path), ['joints[0].bodyB']);
});

test('an omitted restLength measures the anchors, so the spring starts relaxed', () => {
  const { world, bodies } = buildScene({
    gravity: { x: 0, y: 0 },
    bodies: [anchor, ballAt(3, 4)],
    joints: [{ kind: 'spring', bodyA: 'anchor', bodyB: 'ball', stiffness: 50 }],
  });

  const joint = world.getJointList();
  assert.ok(joint instanceof SpringJoint);
  assert.equal(joint.restLength, 5);
  assert.equal(joint.getCollideConnected(), true);

  for (let i = 0; i < 120; i++) world.step(DT);
  const p = bodies[1]!.getPosition();
  assert.ok(Math.abs(p.x - 3) < 1e-9 && Math.abs(p.y - 4) < 1e-9, `relaxed spring moved the ball to ${p.x}, ${p.y}`);
});

test('the frame reports Hooke force on both bodies, a spring segment and ½k(L - L0)²', () => {
  // Stretched by 2 m with k = 10: 20 N pulling the two balls together.
  const { bodies } = buildScene({
    gravity: { x: 0, y: 0 },
    bodies: [
      { ...ballAt(0, 0), id: 'a' },
      { ...ballAt(3, 0), id: 'b' },
    ],
    joints: [{ kind: 'spring', bodyA: 'a', bodyB: 'b', stiffness: 10, restLength: 1 }],
  });

  const frame = buildFrame(bodies, DT);
  const springOn = (i: number) => frame.bodies[i]!.forces.filter((f) => f.kind === 'spring').map((f) => [plain(f.at), plain(f.force)]);

  assert.deepEqual(springOn(0), [[{ x: 0, y: 0 }, { x: 20, y: 0 }]]);
  assert.deepEqual(springOn(1), [[{ x: 3, y: 0 }, { x: -20, y: 0 }]]);
  assert.deepEqual(frame.joints, [{ kind: 'spring', a: { x: 0, y: 0 }, b: { x: 3, y: 0 } }]);
  assert.equal(frame.energy.elastic, 0.5 * 10 * 2 * 2);
});

test('a mass on a spring oscillates with period 2π√(m/k)', () => {
  // k = m(2π)² makes the period exactly 1 s, i.e. 120 steps. Amplitude 0.5 m around x = 1.5.
  const { world, bodies } = buildScene({
    gravity: { x: 0, y: 0 },
    bodies: [anchor, ballAt(2, 0)],
    joints: [{ kind: 'spring', bodyA: 'anchor', bodyB: 'ball', stiffness: BALL_MASS * 4 * Math.PI ** 2, restLength: 1.5 }],
  });
  const x = () => bodies[1]!.getPosition().x;

  for (let i = 0; i < 60; i++) world.step(DT);
  assert.ok(Math.abs(x() - 1) < 0.01, `half a period should reach the far turning point x = 1, got ${x()}`);
  for (let i = 0; i < 60; i++) world.step(DT);
  assert.ok(Math.abs(x() - 2) < 0.01, `a full period should return to x = 2, got ${x()}`);
});

test('hanging under gravity, kinetic + potential + spring energy stays constant', () => {
  const k = 40;
  const { world, bodies } = buildScene({
    gravity: { x: 0, y: -10 },
    bodies: [anchor, ballAt(0, -1)],
    joints: [{ kind: 'spring', bodyA: 'anchor', bodyB: 'ball', stiffness: k }],
  });

  const total = () => {
    const { kinetic, potential, elastic } = buildFrame(bodies, DT).energy;
    return kinetic + potential + elastic;
  };
  const start = total();
  let peakElastic = 0;
  let lowest = 0;
  let worstDrift = 0;
  for (let i = 0; i < 600; i++) {
    world.step(DT);
    peakElastic = Math.max(peakElastic, buildFrame(bodies, DT).energy.elastic);
    lowest = Math.min(lowest, bodies[1]!.getPosition().y);
    worstDrift = Math.max(worstDrift, Math.abs(total() - start));
  }

  // Released relaxed, it sinks to twice the static stretch mg/k below the rest point.
  const stretch = (BALL_MASS * 10) / k;
  assert.ok(Math.abs(lowest - (-1 - 2 * stretch)) < 0.01, `lowest point ${lowest}, expected ${-1 - 2 * stretch}`);
  assert.ok(Math.abs(peakElastic - 0.5 * k * (2 * stretch) ** 2) < 0.02 * peakElastic, `peak spring energy ${peakElastic}`);
  assert.ok(worstDrift < 0.01 * peakElastic, `total energy drifted by ${worstDrift} J against ${peakElastic} J exchanged`);
});

test('a spring on an off-centre anchor applies torque', () => {
  const { world, bodies } = buildScene({
    gravity: { x: 0, y: 0 },
    bodies: [anchor, { id: 'plank', type: 'dynamic', position: { x: 0, y: -3 }, shape: { kind: 'box', halfWidth: 1, halfHeight: 0.1 } }],
    joints: [{ kind: 'spring', bodyA: 'anchor', bodyB: 'plank', localAnchorB: { x: 1, y: 0 }, stiffness: 10, restLength: 1 }],
  });

  world.step(DT);
  // Pulled up and left at its right end, the plank turns counter-clockwise.
  assert.ok(bodies[1]!.getAngularVelocity() > 0, `angular velocity ${bodies[1]!.getAngularVelocity()}`);
});

/** `|| 0` folds -0 into 0, which deepEqual otherwise tells apart. */
function plain(v: { x: number; y: number }): { x: number; y: number } {
  return { x: v.x || 0, y: v.y || 0 };
}

function catchSceneError(fn: () => unknown): SceneError {
  try {
    fn();
  } catch (error) {
    if (error instanceof SceneError) return error;
    throw error;
  }
  throw new Error('expected a SceneError');
}
