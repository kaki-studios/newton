import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseScene } from './schema.js';
import { validateSemantics } from './semantics.js';
import { SceneError } from './error.js';

const ground = {
  id: 'ground',
  type: 'static',
  position: { x: 0, y: 0 },
  shape: { kind: 'edge', a: { x: -5, y: 0 }, b: { x: 5, y: 0 } },
};
const ball = {
  id: 'ball',
  type: 'dynamic',
  position: { x: 0, y: 2 },
  shape: { kind: 'circle', radius: 0.5 },
};

test('a consistent scene passes through unchanged', () => {
  const spec = parseScene({ bodies: [ground, ball] });
  assert.equal(validateSemantics(spec), spec);
});

test('duplicate ids are rejected at the second one', () => {
  const error = catchSceneError(() => validateSemantics(parseScene({ bodies: [ball, ball] })));

  assert.equal(error.kind, 'semantics');
  assert.deepEqual(paths(error), ['bodies[1].id']);
  assert.match(error.message, /duplicate/);
});

test('a joint naming a missing body lists the ids that do exist', () => {
  const error = catchSceneError(() =>
    validateSemantics(
      parseScene({
        bodies: [ground, ball],
        joints: [{ kind: 'distance', bodyA: 'ball', bodyB: 'moon' }],
      }),
    ),
  );

  assert.deepEqual(paths(error), ['joints[0].bodyB']);
  assert.match(error.message, /"moon".*ground, ball/);
});

test('a joint cannot attach a body to itself', () => {
  const error = catchSceneError(() =>
    validateSemantics(
      parseScene({ bodies: [ball], joints: [{ kind: 'distance', bodyA: 'ball', bodyB: 'ball' }] }),
    ),
  );

  assert.deepEqual(paths(error), ['joints[0].bodyB']);
});

test('a dynamic edge is rejected: zero area would give it planck\'s silent 1 kg default', () => {
  const error = catchSceneError(() =>
    validateSemantics(parseScene({ bodies: [{ ...ground, type: 'dynamic' }] })),
  );

  assert.deepEqual(paths(error), ['bodies[0].shape']);
  assert.match(error.message, /static/);
});

test('a zero-length edge is rejected: planck would accept a point nothing collides with', () => {
  const error = catchSceneError(() =>
    validateSemantics(
      parseScene({ bodies: [{ ...ground, shape: { kind: 'edge', a: { x: 1, y: 2 }, b: { x: 1, y: 2 } } }] }),
    ),
  );

  assert.deepEqual(paths(error), ['bodies[0].shape']);
  assert.match(error.message, /identical/);
});

test('every semantic problem is reported in one throw', () => {
  const error = catchSceneError(() =>
    validateSemantics(
      parseScene({
        bodies: [ball, ball, { ...ground, id: 'rod', type: 'dynamic' }],
        joints: [{ kind: 'distance', bodyA: 'ball', bodyB: 'ghost' }],
      }),
    ),
  );

  assert.deepEqual(paths(error), ['bodies[1].id', 'bodies[2].shape', 'joints[0].bodyB']);
});

function catchSceneError(fn: () => unknown): SceneError {
  try {
    fn();
  } catch (error) {
    if (error instanceof SceneError) return error;
    throw error;
  }
  throw new Error('expected the scene to be rejected');
}

function paths(error: SceneError): string[] {
  return error.issues.map((issue) => issue.path);
}
