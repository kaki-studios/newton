import type { Camera, Frame, RenderBody, Renderer, Vec2Like, Viewport } from './types.js';
import { worldToScreen } from './camera.js';

/** Seconds of travel a velocity line represents. */
const VELOCITY_DRAW_SCALE = 1;

/**
 * Canvas2D backend. Only file in the project that touches `<canvas>` — swapping to
 * WebGL/SVG/Three.js means writing one more `Renderer` and deleting this.
 */
export class CanvasRenderer implements Renderer {
  private readonly ctx: CanvasRenderingContext2D;
  private viewport: Viewport = { width: 0, height: 0 };

  constructor(private readonly canvas: HTMLCanvasElement) {
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('canvas 2d context unavailable: is this a real <canvas> element?');
    this.ctx = ctx;
  }

  resize(viewport: Viewport): void {
    this.viewport = viewport;
    const dpr = window.devicePixelRatio || 1;
    this.canvas.width = Math.round(viewport.width * dpr);
    this.canvas.height = Math.round(viewport.height * dpr);
    this.canvas.style.width = `${viewport.width}px`;
    this.canvas.style.height = `${viewport.height}px`;
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  }

  draw(frame: Frame, camera: Camera): void {
    const { ctx } = this;
    ctx.clearRect(0, 0, this.viewport.width, this.viewport.height);

    for (const body of frame.bodies) {
      ctx.strokeStyle = body.isStatic ? '#7a8794' : '#7cc4ff';
      ctx.lineWidth = 2;
      ctx.beginPath();

      for (const shape of body.shapes) {
        if (shape.kind === 'circle') {
          const c = this.toScreen(camera, shape.center);
          const r = shape.radius * camera.scale;
          ctx.moveTo(c.x + r, c.y);
          ctx.arc(c.x, c.y, r, 0, Math.PI * 2);
        } else {
          const [first, ...rest] = shape.points.map((p) => this.toScreen(camera, p));
          if (!first) continue;
          ctx.moveTo(first.x, first.y);
          for (const p of rest) ctx.lineTo(p.x, p.y);
          if (shape.closed) ctx.closePath();
        }
      }

      ctx.stroke();
    }

    // Velocity arrows in a second pass so the red stroke never leaks into outlines.
    // Length is one second of travel in world units; bump `VELOCITY_DRAW_SCALE` for longer arrows.
    ctx.strokeStyle = '#ff4d4d';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    for (const body of frame.bodies) {
      const anchor = bodyAnchor(body);
      if (!anchor) continue;
      const start = this.toScreen(camera, anchor);
      const end = this.toScreen(camera, {
        x: anchor.x + body.linearVelocity.x * VELOCITY_DRAW_SCALE,
        y: anchor.y + body.linearVelocity.y * VELOCITY_DRAW_SCALE,
      });
      ctx.moveTo(start.x, start.y);
      ctx.lineTo(end.x, end.y);
    }
    ctx.stroke();
  }

  private toScreen(camera: Camera, p: Vec2Like): Vec2Like {
    return worldToScreen(camera, this.viewport, p);
  }
}

/** Where a body's velocity line starts: its circle centre, else its first vertex. */
function bodyAnchor(body: RenderBody): Vec2Like | null {
  for (const shape of body.shapes) {
    if (shape.kind === 'circle') return shape.center;
    const first = shape.points[0];
    if (first) return first;
  }
  return null;
}
