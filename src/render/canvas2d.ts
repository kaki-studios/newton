import type { Camera, Energy, ForceVector, Frame, RenderBody, Renderer, Vec2Like, Viewport } from './types.js';
import { worldToScreen } from './camera.js';

/** Seconds of travel a velocity line represents. */
const VELOCITY_DRAW_SCALE = 0.2;

const JOINT_COLOR = '#ffffff';

/** Metres of arrow per newton; 1 N/kg of gravity is ~0.1 m, so arrows stay short. */
const FORCE_DRAW_SCALE = 0.1;

const FORCE_COLORS: Record<ForceVector['kind'], string> = {
  gravity: '#7dff8a',
  joint: '#ffc857',
  contact: '#d97bff',
};

/** Steps of energy history kept: 600 steps at 120 Hz is a 5 s window. */
const ENERGY_HISTORY = 600;

const ENERGY_PANEL = { x: 12, y: 12, width: 280, height: 120 };

const ENERGY_COLORS: Record<keyof Energy | 'total', string> = {
  kinetic: '#7dff8a',
  potential: '#ffc857',
  total: '#ffffff',
};

/**
 * Canvas2D backend. Only file in the project that touches `<canvas>` — swapping to
 * WebGL/SVG/Three.js means writing one more `Renderer` and deleting this.
 */
export class CanvasRenderer implements Renderer {
  private readonly ctx: CanvasRenderingContext2D;
  private viewport: Viewport = { width: 0, height: 0 };
  /** Energy per step, oldest first. Render state: the sim only ever hands over a value. */
  private readonly energyHistory: Energy[] = [];

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

    // Joints are a plain segment between anchors, on top of the bodies they link.
    ctx.strokeStyle = JOINT_COLOR;
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    for (const joint of frame.joints) {
      const a = this.toScreen(camera, joint.a);
      const b = this.toScreen(camera, joint.b);
      ctx.moveTo(a.x, a.y);
      ctx.lineTo(b.x, b.y);
    }
    ctx.stroke();

    // Velocity arrows in a second pass so the red stroke never leaks into outlines.
    // Length is `VELOCITY_DRAW_SCALE` world metres per m/s.
    for (const body of frame.bodies) {
      const anchor = bodyAnchor(body);
      if (!anchor) continue;
      const start = this.toScreen(camera, anchor);
      const end = this.toScreen(camera, {
        x: anchor.x + body.linearVelocity.x * VELOCITY_DRAW_SCALE,
        y: anchor.y + body.linearVelocity.y * VELOCITY_DRAW_SCALE,
      });
      this.arrow(start, end, '#ff4d4d');
    }

    // Force arrows last, in their own pass: one colour per force kind, arrowheads so
    // the direction is unambiguous. `FORCE_DRAW_SCALE` tunes world length per newton.
    for (const body of frame.bodies) {
      for (const vector of body.forces) {
        const start = this.toScreen(camera, vector.at);
        const end = this.toScreen(camera, {
          x: vector.at.x + vector.force.x * FORCE_DRAW_SCALE,
          y: vector.at.y + vector.force.y * FORCE_DRAW_SCALE,
        });
        this.arrow(start, end, FORCE_COLORS[vector.kind]);
      }
    }

    this.drawEnergyChart(frame.energy);
  }

  /**
   * Top-left energy chart: kinetic, potential and total over the last `ENERGY_HISTORY`
   * steps, newest on the right. The y axis auto-scales to whatever is on screen and
   * the absolute values are printed, so a bar without ticks still reads exactly.
   */
  private drawEnergyChart(energy: Energy): void {
    this.energyHistory.push(energy);
    if (this.energyHistory.length > ENERGY_HISTORY) this.energyHistory.shift();

    const { ctx } = this;
    const { x, y, width, height } = ENERGY_PANEL;
    const total = energy.kinetic + energy.potential;

    let min = 0;
    let max = 0;
    for (const e of this.energyHistory) {
      const t = e.kinetic + e.potential;
      min = Math.min(min, e.kinetic, e.potential, t);
      max = Math.max(max, e.kinetic, e.potential, t);
    }
    const span = max - min || 1;

    ctx.fillStyle = '#0c1116cc';
    ctx.fillRect(x, y, width, height);
    ctx.strokeStyle = '#2a3542';
    ctx.lineWidth = 1;
    ctx.strokeRect(x, y, width, height);

    // Zero line, so kinetic and potential keep their sign visible.
    const zeroY = y + height - ((0 - min) / span) * height;
    ctx.beginPath();
    ctx.moveTo(x, zeroY);
    ctx.lineTo(x + width, zeroY);
    ctx.stroke();

    const count = this.energyHistory.length;
    const plot = (key: keyof Energy | 'total', color: string): void => {
      ctx.strokeStyle = color;
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      this.energyHistory.forEach((e, i) => {
        const value = key === 'total' ? e.kinetic + e.potential : e[key];
        // Newest sample hugs the right edge; a short history fills in from there.
        const px = x + width - ((count - 1 - i) / (ENERGY_HISTORY - 1)) * width;
        const py = y + height - ((value - min) / span) * height;
        if (i === 0) ctx.moveTo(px, py);
        else ctx.lineTo(px, py);
      });
      ctx.stroke();
    };

    plot('potential', ENERGY_COLORS.potential);
    plot('kinetic', ENERGY_COLORS.kinetic);
    plot('total', ENERGY_COLORS.total);

    ctx.font = '11px ui-monospace, monospace';
    ctx.fillStyle = ENERGY_COLORS.total;
    ctx.textBaseline = 'alphabetic';
    ctx.fillText(`total ${total.toFixed(1)} J`, x + 6, y + 14);
    ctx.fillStyle = ENERGY_COLORS.kinetic;
    ctx.fillText(`kinetic ${energy.kinetic.toFixed(1)} J`, x + 6, y + 28);
    ctx.fillStyle = ENERGY_COLORS.potential;
    ctx.fillText(`potential ${energy.potential.toFixed(1)} J`, x + 6, y + 42);
  }

  /** Line plus a head, both stroked in one colour. */
  private arrow(start: Vec2Like, end: Vec2Like, color: string): void {
    const { ctx } = this;
    const dx = end.x - start.x;
    const dy = end.y - start.y;
    const length = Math.hypot(dx, dy);
    if (length < 1) return; // sub-pixel force: drawing it is just noise

    ctx.strokeStyle = color;
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    ctx.moveTo(start.x, start.y);
    ctx.lineTo(end.x, end.y);

    const head = Math.min(9, length * 0.4);
    const ux = dx / length;
    const uy = dy / length;
    ctx.moveTo(end.x, end.y);
    ctx.lineTo(end.x - ux * head - uy * head * 0.5, end.y - uy * head + ux * head * 0.5);
    ctx.moveTo(end.x, end.y);
    ctx.lineTo(end.x - ux * head + uy * head * 0.5, end.y - uy * head - ux * head * 0.5);
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
