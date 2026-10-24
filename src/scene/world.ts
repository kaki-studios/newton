import { Box, Circle, DistanceJoint, Edge, World, type Body, type Shape } from 'planck';
import type { BodySpec, SceneSpec, ShapeSpec } from './schema.js';

export interface Scene {
  world: World;
  /** Bodies in document order; the renderer needs them, ids travel via userData. */
  bodies: Body[];
}

/**
 * The only place a planck `World` is constructed. Takes a spec that passed both gates
 * (`parseScene` then `validateSemantics`) and turns it into bodies and joints — no
 * validation happens here, so anything invalid has to be caught upstream.
 */
export function buildWorld(spec: SceneSpec): Scene {
  const world = new World({ gravity: { x: spec.gravity.x, y: spec.gravity.y } });
  const byId = new Map<string, Body>();

  // `validateSemantics` guarantees unique ids; the map assertion below keeps that
  // promise local, so a future caller skipping the gate fails loudly instead of
  // silently collapsing two bodies into one.
  for (const bodySpec of spec.bodies) {
    const body = createBody(world, bodySpec);
    if (byId.has(bodySpec.id)) throw new Error(`buildWorld: duplicate body id "${bodySpec.id}" — run validateSemantics first`);
    byId.set(bodySpec.id, body);
  }

  for (const jointSpec of spec.joints) {
    const bodyA = byId.get(jointSpec.bodyA);
    const bodyB = byId.get(jointSpec.bodyB);
    if (!bodyA || !bodyB) {
      throw new Error(`internal: joint references missing body "${bodyA ? jointSpec.bodyB : jointSpec.bodyA}"`);
    }
    // Local anchors go straight through; planck transforms them per body. A missing
    // length makes planck measure the current anchor distance, which is our default.
    world.createJoint(
      new DistanceJoint({
        bodyA,
        bodyB,
        localAnchorA: jointSpec.localAnchorA,
        localAnchorB: jointSpec.localAnchorB,
        length: jointSpec.length,
        collideConnected: jointSpec.collideConnected,
      }),
    );
  }

  return { world, bodies: spec.bodies.map((bodySpec) => byId.get(bodySpec.id)!) };
}

function createBody(world: World, spec: BodySpec): Body {
  const body = world.createBody({ type: spec.type, position: spec.position, angle: spec.angle });
  // Ids are the renderer's only handle on a body, so they must survive into the frame.
  body.setUserData(spec.id);
  body.createFixture(toShape(spec.shape), { density: spec.density });
  return body;
}

/** Shapes are authored in the body's local frame, which is exactly what planck expects. */
function toShape(shape: ShapeSpec): Shape {
  switch (shape.kind) {
    case 'circle':
      return new Circle(shape.radius);
    case 'box':
      return new Box(shape.halfWidth, shape.halfHeight);
    case 'edge':
      return new Edge(shape.a, shape.b);
  }
}
