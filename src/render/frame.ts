import { DistanceProxy, type Body, type Shape } from 'planck';
import type { Frame, ShapeGeometry, Vec2Like } from './types.js';

/**
 * The only place planck types are read. Every shape is flattened through
 * `DistanceProxy`, which exposes vertices for all four shape kinds uniformly —
 * the alternative is a `switch` on shape type per backend.
 *
 * Must be called after stepping and before drawing; the returned frame is a
 * plain value, so a backend can hold onto it without keeping planck alive.
 */
export function buildFrame(bodies: Body[]): Frame {
  return {
    bodies: bodies.map((body) => ({
      id: String(body.getUserData() ?? ''),
      isStatic: body.getType() === 'static',
      shapes: bodyShapes(body),
      linearVelocity: body.getLinearVelocity(),
    })),
  };
}

// One proxy for the whole process: JS is single-threaded and `set` is cheap, so
// re-allocating per frame would only add garbage.
const proxy = new DistanceProxy();

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
