import { test } from 'node:test';
import assert from 'node:assert/strict';
import type { Body } from 'planck';
import { buildFrame } from '../render/frame.js';
import { buildWorld } from './world.js';
import { parseScene } from './schema.js';
import { validateSemantics } from './semantics.js';
import type { SceneSpec } from './schema.js';

const DT = 1 / 60;

function scene(doc: unknown) {
  return buildWorld(validateSemantics(parseScene(doc)));
}

test('bodies keep their document order, types and ids in userData', () => {
  const { world, bodies } = scene({
    bodies: [
      { id: 'ground', type: 'static', position: { x: 0, y: 0 }, shape: { kind: 'edge', a: { x: -5, y: 0 }, b: { x: 5, y: 0 } } },
      { id: 'ball', type: 'dynamic', position: { x: 0, y: 2 }, shape: { kind: 'circle', radius: 0.5 } },
    ],
  });

  assert.equal(world.getBodyCount(), 2);
  assert.deepEqual(
    bodies.map((b) => [b.getUserData(), b.getType()]),
    [['ground', 'static'], ['ball', 'dynamic']],
  );
});

test('gravity comes from the document', () => {
  const { world } = scene({
    gravity: { x: 0, y: -3.5 },
    bodies: [{ id: 'ball', type: 'dynamic', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.5 } }],
  });

  assert.deepEqual(vec(world.getGravity()), { x: 0, y: -3.5 });
});

test('position, angle, density and every shape kind reach planck', () => {
  const { bodies } = scene({
    bodies: [
      {
        id: 'stack',
        type: 'dynamic',
        position: { x: 1, y: 2 },
        angle: Math.PI / 4,
        density: 2,
        shape: { kind: 'box', halfWidth: 1, halfHeight: 0.5 },
      },
      { id: 'peg', type: 'static', position: { x: -3, y: 0 }, shape: { kind: 'circle', radius: 0.25 } },
      { id: 'floor', type: 'static', position: { x: 0, y: -1 }, shape: { kind: 'edge', a: { x: -5, y: 0 }, b: { x: 5, y: 0 } } },
    ],
  });

  const [stack, peg, floor] = bodies;
  assert.deepEqual(vec(stack!.getPosition()), { x: 1, y: 2 });
  assert.equal(stack!.getAngle(), Math.PI / 4);
  // Box: area 2*1 * 2*0.5 = 2 m², density 2 kg/m².
  assert.equal(stack!.getMass(), 4);
  assert.equal(shapeType(stack!), 'polygon');
  assert.equal(shapeType(peg!), 'circle');
  assert.equal(shapeType(floor!), 'edge');
});

test('a distance joint anchors on the local points of its bodies', () => {
  // Anchors are local, so a rotated body still pins the same spot on itself.
  const { world, bodies } = scene({
    gravity: { x: 0, y: 0 },
    bodies: [
      { id: 'arm', type: 'static', position: { x: 1, y: 2 }, angle: Math.PI / 2, shape: { kind: 'circle', radius: 0.1 } },
      { id: 'ball', type: 'dynamic', position: { x: 4, y: 6 }, shape: { kind: 'circle', radius: 0.5 } },
    ],
    joints: [{ kind: 'distance', bodyA: 'arm', bodyB: 'ball', localAnchorA: { x: 1, y: 0 } }],
  });

  assert.equal(world.getJointCount(), 1);
  const joint = world.getJointList()!;
  // Local (1,0) on a body at (1,2) rotated 90° is world (1,3).
  assert.deepEqual(vec(joint.getAnchorA()), { x: 1, y: 3 });
  assert.deepEqual(vec(joint.getAnchorB()), { x: 4, y: 6 });
  assert.equal(bodies.length, 2);
});

test('an omitted length measures the anchors, so the ball starts in equilibrium', () => {
  const { world, bodies } = scene({
    gravity: { x: 0, y: -10 },
    bodies: [
      { id: 'anchor', type: 'static', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.1 } },
      { id: 'ball', type: 'dynamic', position: { x: 0, y: -2 }, shape: { kind: 'circle', radius: 0.4 } },
    ],
    joints: [{ kind: 'distance', bodyA: 'anchor', bodyB: 'ball' }],
  });

  for (let i = 0; i < 120; i++) world.step(DT);
  assert.ok(Math.abs(bodies[1]!.getPosition().y + 2) < 0.01, `ball drifted to y=${bodies[1]!.getPosition().y}`);
});

test('an explicit length holds the body at that distance', () => {
  const { world, bodies } = scene({
    gravity: { x: 0, y: 0 },
    bodies: [
      { id: 'anchor', type: 'static', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.1 } },
      { id: 'ball', type: 'dynamic', position: { x: 0, y: -1 }, shape: { kind: 'circle', radius: 0.4 } },
    ],
    joints: [{ kind: 'distance', bodyA: 'anchor', bodyB: 'ball', length: 3 }],
  });

  for (let i = 0; i < 240; i++) world.step(DT);
  const y = bodies[1]!.getPosition().y;
  assert.ok(Math.abs(Math.abs(y) - 3) < 0.01, `expected |y| = 3, got ${y}`);
});

test('length 0 is accepted as a pin that holds a body at one point', () => {
  const { world, bodies } = scene({
    gravity: { x: 0, y: -10 },
    bodies: [
      { id: 'anchor', type: 'static', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.1 } },
      { id: 'ball', type: 'dynamic', position: { x: 1, y: 0 }, shape: { kind: 'circle', radius: 0.4 } },
    ],
    joints: [{ kind: 'distance', bodyA: 'anchor', bodyB: 'ball', length: 0 }],
  });

  for (let i = 0; i < 600; i++) world.step(DT);
  const p = vec(bodies[1]!.getPosition());
  assert.ok(Math.hypot(p.x, p.y) < 0.01, `pin let the body drift to ${JSON.stringify(p)}`);
});

test('collideConnected is carried through to the joint', () => {
  const joints = (collideConnected: boolean) =>
    scene({
      bodies: [
        { id: 'a', type: 'static', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.1 } },
        { id: 'b', type: 'dynamic', position: { x: 0, y: -1 }, shape: { kind: 'circle', radius: 0.1 } },
      ],
      joints: [{ kind: 'distance', bodyA: 'a', bodyB: 'b', collideConnected }],
    }).world.getJointList()!;

  assert.equal(joints(true).getCollideConnected(), true);
  assert.equal(joints(false).getCollideConnected(), false);
});

test('built bodies flow through buildFrame with their ids intact', () => {
  const spec: SceneSpec = validateSemantics(
    parseScene({
      bodies: [
        { id: 'ground', type: 'static', position: { x: 0, y: 0 }, shape: { kind: 'edge', a: { x: -5, y: 0 }, b: { x: 5, y: 0 } } },
        { id: 'ball', type: 'dynamic', position: { x: 0, y: 1.2 }, shape: { kind: 'circle', radius: 0.5 } },
      ],
    }),
  );
  const { world, bodies } = buildWorld(spec);

  for (let i = 0; i < 240; i++) world.step(DT);
  const frame = buildFrame(bodies, DT);

  assert.deepEqual(frame.bodies.map((b) => b.id), ['ground', 'ball']);
  // The renderer contract needs no change: scene documents reach the canvas as-is.
  const resting = frame.bodies[1]!;
  assert.ok(resting.forces.some((f) => f.kind === 'contact'), 'ball should rest on the ground');
});

test('a thrown-away spec cannot leak defaults into the next scene', () => {
  const first = scene({
    gravity: { x: 1, y: 2 },
    bodies: [{ id: 'a', type: 'dynamic', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.1 } }],
  });
  const second = scene({
    bodies: [{ id: 'a', type: 'dynamic', position: { x: 0, y: 0 }, shape: { kind: 'circle', radius: 0.1 } }],
  });

  assert.deepEqual(vec(first.world.getGravity()), { x: 1, y: 2 });
  assert.deepEqual(vec(second.world.getGravity()), { x: 0, y: -9.81 });
});

function shapeType(body: Body): string {
  return body.getFixtureList()!.getShape().getType();
}

/**
 * Planck hands back `Vec2` instances, whose prototype fails deepEqual against literals.
 * Copy to a plain literal and round, so float noise does not defeat exact comparison.
 */
function vec(v: { x: number; y: number }): { x: number; y: number } {
  return { x: Math.round(v.x * 1e6) / 1e6 || 0, y: Math.round(v.y * 1e6) / 1e6 || 0 };
}
