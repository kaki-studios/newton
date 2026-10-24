/**
 * Renderer-agnostic scene snapshot. Nothing here knows about canvas, WebGL or planck —
 * a backend only has to consume a `Frame` plus a `Camera`.
 */

export interface Vec2Like {
  x: number;
  y: number;
}

/** A shape flattened to plain world-space geometry. */
export type ShapeGeometry =
  | { kind: 'polyline'; points: Vec2Like[]; closed: boolean }
  | { kind: 'circle'; center: Vec2Like; radius: number };

/** A force in newtons acting at a world point, already resolved into the body's frame. */
export interface ForceVector {
  /** Where the force acts: centre of mass for gravity, the anchor for joints, the contact point. */
  at: Vec2Like;
  /** World-space force in newtons. */
  force: Vec2Like;
  /** Origin of the force, so a backend can colour it. */
  kind: 'gravity' | 'joint' | 'contact';
}

export interface RenderBody {
  id: string;
  isStatic: boolean;
  linearVelocity: Vec2Like;
  shapes: ShapeGeometry[];
  /** Forces on dynamic bodies this step; empty for static/kinematic ones. */
  forces: ForceVector[];
}

/** A joint flattened to the segment between its two world-space anchors. */
export interface RenderJoint {
  /** Anchor on body A. */
  a: Vec2Like;
  /** Anchor on body B. */
  b: Vec2Like;
}

/** Mechanical energy in joules, summed over the dynamic bodies this step. */
export interface Energy {
  kinetic: number;
  /** Relative: measured against the world origin, so only changes matter. */
  potential: number;
}

/** One frame worth of geometry. Rebuilt each step, never mutated. */
export interface Frame {
  bodies: RenderBody[];
  joints: RenderJoint[];
  energy: Energy;
}

/** World is metres; `scale` is pixels per metre. */
export interface Camera {
  center: Vec2Like;
  scale: number;
}

export interface Viewport {
  width: number;
  height: number;
}

/** The only contract a rendering backend has to satisfy. */
export interface Renderer {
  resize(viewport: Viewport): void;
  draw(frame: Frame, camera: Camera): void;
}
