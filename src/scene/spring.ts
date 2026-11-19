import { Joint, Vec2, type Body, type JointDef, type Vec2Value, type World } from 'planck';

export interface SpringJointDef {
  bodyA: Body;
  bodyB: Body;
  /** Local to body A. */
  localAnchorA: Vec2Value;
  /** Local to body B. */
  localAnchorB: Vec2Value;
  /** N/m. */
  stiffness: number;
  /** Metres. Omit to use the anchors' world distance at construction. */
  restLength?: number;
  collideConnected: boolean;
}

/**
 * A Hooke's-law spring between two anchors: `F = -k (L - L0) û` on body B, `-F` on body A.
 *
 * planck has no plugin API for new constraint types, so this joint adds no constraint rows —
 * the solver callbacks are no-ops — and the force is applied with `Body.applyForce` before each
 * step (`installSprings`). Being a `Joint` keeps it in planck's joint lists, so islands,
 * sleeping, `collideConnected` and the frame builder treat it like any other joint.
 */
export class SpringJoint extends Joint {
  readonly localAnchorA: Vec2;
  readonly localAnchorB: Vec2;
  readonly stiffness: number;
  readonly restLength: number;

  constructor(def: SpringJointDef) {
    super(def);
    this.localAnchorA = Vec2.clone(def.localAnchorA);
    this.localAnchorB = Vec2.clone(def.localAnchorB);
    this.stiffness = def.stiffness;
    this.restLength = def.restLength ?? Vec2.distance(this.getAnchorA(), this.getAnchorB());
  }

  getAnchorA(): Vec2 {
    return this.getBodyA().getWorldPoint(this.localAnchorA);
  }

  getAnchorB(): Vec2 {
    return this.getBodyB().getWorldPoint(this.localAnchorB);
  }

  /** The spring force on body B, in newtons. It is a force already, so `inv_dt` is unused. */
  getReactionForce(_inv_dt: number): Vec2 {
    const d = Vec2.sub(this.getAnchorB(), this.getAnchorA());
    const length = d.length();
    // Coincident anchors have no direction; with L0 = 0 the force is zero there anyway.
    if (length < 1e-9) return Vec2.zero();
    return Vec2.mulNumVec2((-this.stiffness * (length - this.restLength)) / length, d);
  }

  /** A force at the anchor, not a couple: its torque about B is already in the force. */
  getReactionTorque(_inv_dt: number): number {
    return 0;
  }

  /** Joules: ½k(L - L0)². */
  getElasticEnergy(): number {
    const stretch = Vec2.distance(this.getAnchorA(), this.getAnchorB()) - this.restLength;
    return 0.5 * this.stiffness * stretch * stretch;
  }

  /** Accumulates the force for the coming step; planck clears forces after every step. */
  applyForce(): void {
    const force = this.getReactionForce(0);
    this.getBodyB().applyForce(force, this.getAnchorB(), true);
    this.getBodyA().applyForce(Vec2.neg(force), this.getAnchorA(), true);
  }

  initVelocityConstraints(): void {}
  solveVelocityConstraints(): void {}
  solvePositionConstraints(): boolean {
    return true;
  }
  _reset(_def: Partial<JointDef>): void {}
}

/** Applies every spring's force at the start of each `world.step`. Call once per world. */
export function installSprings(world: World): void {
  // `pre-step` is published by `World.step` but missing from planck's `on` overloads.
  const on = world.on as unknown as (name: 'pre-step', listener: () => void) => World;
  on.call(world, 'pre-step', () => {
    for (let joint = world.getJointList(); joint; joint = joint.getNext()) {
      if (joint instanceof SpringJoint) joint.applyForce();
    }
  });
}
