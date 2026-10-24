import { DistanceProxy, WorldManifold, type Body, type Joint, type Shape } from 'planck';
import type { Energy, ForceVector, Frame, RenderJoint, ShapeGeometry, Vec2Like } from './types.js';

/**
 * The only place planck types are read. Every shape is flattened through
 * `DistanceProxy`, which exposes vertices for all four shape kinds uniformly —
 * the alternative is a `switch` on shape type per backend.
 *
 * Must be called after stepping and before drawing; the returned frame is a
 * plain value, so a backend can hold onto it without keeping planck alive.
 *
 * `dt` is the step size that was just simulated: contact impulses are per-step,
 * and `getReactionForce` wants the same inverse timestep the solver used.
 */
export function buildFrame(bodies: Body[], dt: number): Frame {
  return {
    bodies: bodies.map((body) => ({
      id: String(body.getUserData() ?? ''),
      isStatic: body.getType() === 'static',
      shapes: bodyShapes(body),
      linearVelocity: toVec2(body.getLinearVelocity()),
      forces: bodyForces(body, dt),
    })),
    joints: frameJoints(bodies),
    energy: frameEnergy(bodies),
  };
}

/**
 * Total mechanical energy in joules: ½mv² + ½Iω² + U.
 *
 * `U = -m·g·r` rather than plain `mgh`, so a non-vertical gravity field works too;
 * it is measured from the world origin, which makes the absolute value arbitrary
 * but every change physical. Static bodies carry no energy, so they are skipped.
 */
function frameEnergy(bodies: Body[]): Energy {
  const gravity = bodies[0]?.getWorld().getGravity();
  if (!gravity) return { kinetic: 0, potential: 0 };

  let kinetic = 0;
  let potential = 0;
  for (const body of bodies) {
    if (!body.isDynamic()) continue;
    const mass = body.getMass();
    const v = body.getLinearVelocity();
    const w = body.getAngularVelocity();
    kinetic += 0.5 * mass * (v.x * v.x + v.y * v.y) + 0.5 * body.getInertia() * w * w;

    const c = body.getWorldCenter();
    potential -= body.getGravityScale() * mass * (gravity.x * c.x + gravity.y * c.y);
  }

  return { kinetic, potential };
}

/** Each joint once, as the segment between its anchors — joints sit on both bodies' lists. */
function frameJoints(bodies: Body[]): RenderJoint[] {
  const joints: RenderJoint[] = [];
  const seen = new Set<Joint>();

  for (const body of bodies) {
    for (let edge = body.getJointList(); edge; edge = edge.next) {
      const joint = edge.joint;
      if (!joint || seen.has(joint)) continue;
      seen.add(joint);
      joints.push({ a: toVec2(joint.getAnchorA()), b: toVec2(joint.getAnchorB()) });
    }
  }

  return joints;
}

/**
 * Every force planck solved for on this body, in newtons. Static and kinematic
 * bodies accumulate nothing, so they report no forces. Torques are dropped —
 * a straight line cannot show one. Gravity is reported from the world, not the
 * solver, because planck never materialises it as a force.
 */
function bodyForces(body: Body, dt: number): ForceVector[] {
  if (!body.isDynamic()) return [];

  const invDt = 1 / dt;
  const forces: ForceVector[] = [];

  const gravity = body.getWorld().getGravity();
  const weight = body.getMass() * body.getGravityScale();
  forces.push({
    kind: 'gravity',
    at: toVec2(body.getWorldCenter()),
    force: { x: gravity.x * weight, y: gravity.y * weight },
  });

  for (let edge = body.getJointList(); edge; edge = edge.next) {
    const joint = edge.joint;
    if (!joint) continue;
    // Planck reports the reaction on bodyB at anchorB; bodyA feels the opposite.
    const onB = joint.getBodyB() === body;
    const reaction = joint.getReactionForce(invDt);
    const sign = onB ? 1 : -1;
    forces.push({
      kind: 'joint',
      at: toVec2(onB ? joint.getAnchorB() : joint.getAnchorA()),
      force: { x: sign * reaction.x, y: sign * reaction.y },
    });
  }

  // Contact impulses are stored once per step in the manifold points, so they
  // are forces only after dividing by dt. Read after stepping, never during.
  for (let edge = body.getContactList(); edge; edge = edge.next) {
    const contact = edge.contact;
    if (!contact.isTouching()) continue;
    const manifold = contact.getManifold();
    const world = contact.getWorldManifold(worldManifold);
    if (!world || world.pointCount === 0) continue;

    const onB = contact.getFixtureB().getBody() === body;
    const sign = onB ? 1 : -1;
    const n = world.normal; // points from A to B
    const count = Math.min(world.pointCount, manifold.points.length, world.points.length);
    for (let i = 0; i < count; i++) {
      const impulse = manifold.points[i];
      const at = world.points[i];
      if (!impulse || !at) continue;
      // Same combination planck warm-starts with: normalImpulse*n + tangentImpulse*cross(n, 1).
      const x = impulse.normalImpulse * n.x + impulse.tangentImpulse * n.y;
      const y = impulse.normalImpulse * n.y - impulse.tangentImpulse * n.x;
      forces.push({ kind: 'contact', at: toVec2(at), force: { x: sign * x * invDt, y: sign * y * invDt } });
    }
  }

  return forces;
}

// One proxy for the whole process: JS is single-threaded and `set` is cheap, so
// re-allocating per frame would only add garbage.
const proxy = new DistanceProxy();

// Same reasoning for the contact manifold: values are copied out immediately, so
// one instance can serve every contact of every frame.
const worldManifold = new WorldManifold();

function bodyShapes(body: Body): ShapeGeometry[] {
  const shapes: ShapeGeometry[] = [];
  for (let fixture = body.getFixtureList(); fixture; fixture = fixture.getNext()) {
    const geometry = shapeGeometry(fixture.getShape(), body);
    if (geometry) shapes.push(geometry);
  }
  return shapes;
}

function shapeGeometry(shape: Shape, body: Body): ShapeGeometry | null {
  if (shape.getType() === 'circle') {
    // A circle's single proxy vertex is its local centre.
    proxy.set(shape, 0);
    return {
      kind: 'circle',
      center: toVec2(body.getWorldPoint(proxy.getVertex(0))),
      radius: shape.getRadius(),
    };
  }

  proxy.set(shape, 0);
  const count = proxy.getVertexCount();
  if (count === 0) return null;

  const points: Vec2Like[] = [];
  for (let i = 0; i < count; i++) {
    points.push(toVec2(body.getWorldPoint(proxy.getVertex(i))));
  }

  // An edge is a single open segment; polygons and chains are closed outlines.
  return { kind: 'polyline', points, closed: shape.getType() !== 'edge' };
}

function toVec2(v: { x: number; y: number }): Vec2Like {
  return { x: v.x, y: v.y };
}
