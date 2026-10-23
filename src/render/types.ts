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

export interface RenderBody {
  id: string;
  isStatic: boolean;
  linearVelocity: Vec2Like;
  shapes: ShapeGeometry[];
}

/** One frame worth of geometry. Rebuilt each step, never mutated. */
export interface Frame {
  bodies: RenderBody[];
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
