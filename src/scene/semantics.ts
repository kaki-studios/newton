import { SceneError, type SceneIssue } from './error.js';
import type { SceneSpec } from './schema.js';

/**
 * Semantic gate: everything zod cannot see because it needs the whole document.
 *
 * Input is a syntactically valid `SceneSpec`; output is the same value, now guaranteed
 * safe for `buildWorld` — so planck never has to be defensive about it. Rules stay
 * conservative: reject only what would crash planck or silently change what the author
 * asked for.
 */
export function validateSemantics(spec: SceneSpec): SceneSpec {
  const issues: SceneIssue[] = [];
  const ids = new Set<string>();

  spec.bodies.forEach((body, i) => {
    if (ids.has(body.id)) {
      issues.push({ path: `bodies[${i}].id`, message: `duplicate body id "${body.id}": ids identify bodies in the renderer and must be unique` });
    }
    ids.add(body.id);

    // An edge is a segment: zero area, so planck would silently hand a dynamic body
    // the default mass of 1 kg — a body that looks weightless but is not.
    if (body.type === 'dynamic' && body.shape.kind === 'edge') {
      issues.push({
        path: `bodies[${i}].shape`,
        message: `a dynamic body needs area to get mass from density; an edge has none — make "${body.id}" static or give it a box/circle shape`,
      });
    }

    // A zero-length edge is a point: planck accepts it but nothing ever collides with
    // it, so the body would silently do nothing.
    if (body.shape.kind === 'edge' && body.shape.a.x === body.shape.b.x && body.shape.a.y === body.shape.b.y) {
      issues.push({
        path: `bodies[${i}].shape`,
        message: `edge endpoints are identical, so the shape is a point nothing can collide with — give "${body.id}" two distinct endpoints`,
      });
    }
  });

  const known = [...ids].join(', ');
  spec.joints.forEach((joint, i) => {
    for (const [name, id] of [['bodyA', joint.bodyA], ['bodyB', joint.bodyB]] as const) {
      if (!ids.has(id)) {
        issues.push({ path: `joints[${i}].${name}`, message: `no body with id "${id}"; known ids: ${known}` });
      }
    }
    if (joint.bodyA === joint.bodyB) {
      issues.push({ path: `joints[${i}].bodyB`, message: `a joint needs two different bodies, both are "${joint.bodyA}"` });
    }
  });

  if (issues.length > 0) throw new SceneError('semantics', issues);
  return spec;
}
