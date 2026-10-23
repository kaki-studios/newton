import type { Camera, Vec2Like, Viewport } from './types.js';

/**
 * World metres -> screen pixels. Y is flipped: planck's +y points up, canvas +y points down.
 * Pure math so it is testable and reusable by any backend (WebGL can reuse it verbatim).
 */
export function worldToScreen(camera: Camera, viewport: Viewport, p: Vec2Like): Vec2Like {
  return {
    x: viewport.width / 2 + (p.x - camera.center.x) * camera.scale,
    y: viewport.height / 2 - (p.y - camera.center.y) * camera.scale,
  };
}
