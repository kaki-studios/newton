import { test } from 'node:test';
import assert from 'node:assert/strict';
import { parseScene, parseSceneJson } from './schema.js';
import { SceneError } from './error.js';

const ball = {
  id: 'ball',
  type: 'dynamic',
  position: { x: 0, y: 2 },
  shape: { kind: 'circle', radius: 0.5 },
};

test('defaults fill in gravity, angle, density and joints', () => {
  const spec = parseScene({ bodies: [ball] });

  assert.deepEqual(spec.gravity, { x: 0, y: -9.81 });
  assert.deepEqual(spec.joints, []);
  assert.equal(spec.bodies[0]!.angle, 0);
  assert.equal(spec.bodies[0]!.density, 1);
});

test('defaults are fresh values, not shared between parses', () => {
  const a = parseScene({ bodies: [ball] });
  const b = parseScene({ bodies: [ball] });

  a.gravity.y = 5;
  a.joints.push({ kind: 'distance', bodyA: 'ball', bodyB: 'ball', collideConnected: false, localAnchorA: { x: 0, y: 0 }, localAnchorB: { x: 0, y: 0 } });
  assert.deepEqual(b.gravity, { x: 0, y: -9.81 });
  assert.deepEqual(b.joints, []);
});

test('ids are trimmed so " ball " and "ball" cannot silently become two bodies', () => {
  assert.equal(parseScene({ bodies: [{ ...ball, id: '  ball  ' }] }).bodies[0]!.id, 'ball');
});

test('a blank id is rejected at its own path', () => {
  const error = catchSceneError(() => parseScene({ bodies: [{ ...ball, id: '   ' }] }));

  assert.equal(error.kind, 'syntax');
  assert.deepEqual(paths(error), ['bodies[0].id']);
});

test('an unknown shape kind names the field and the allowed kinds', () => {
  const error = catchSceneError(() =>
    parseScene({ bodies: [{ ...ball, shape: { kind: 'sphere', radius: 0.5 } }] }),
  );

  assert.deepEqual(paths(error), ['bodies[0].shape.kind']);
  assert.match(error.message, /circle.*box.*edge/);
});

test('a missing position reports the exact component', () => {
  const error = catchSceneError(() => parseScene({ bodies: [{ ...ball, position: { x: 0 } }] }));

  assert.deepEqual(paths(error), ['bodies[0].position.y']);
});

test('a misspelled key is an error instead of being dropped', () => {
  const error = catchSceneError(() => parseScene({ bodies: [{ ...ball, radius: 0.5 }] }));

  assert.deepEqual(paths(error), ['bodies[0]']);
  assert.match(error.message, /radius/);
});

test('every syntax problem is reported in one throw', () => {
  const error = catchSceneError(() =>
    parseScene({
      bodies: [{ ...ball, id: '' }, { ...ball, id: 'b', shape: { kind: 'circle', radius: -1 } }],
    }),
  );

  assert.deepEqual(paths(error), ['bodies[0].id', 'bodies[1].shape.radius']);
});

test('an empty body list is rejected: a scene needs something to show', () => {
  assert.deepEqual(paths(catchSceneError(() => parseScene({ bodies: [] }))), ['bodies']);
});

test('numbers must be finite, so NaN and Infinity never reach planck', () => {
  const error = catchSceneError(() =>
    parseScene({ bodies: [{ ...ball, position: { x: Number.NaN, y: Infinity } }] }),
  );

  assert.deepEqual(paths(error), ['bodies[0].position.x', 'bodies[0].position.y']);
});

test('a joint without a kind is rejected rather than guessed', () => {
  const error = catchSceneError(() => parseScene({ bodies: [ball], joints: [{ bodyA: 'ball', bodyB: 'ball' }] }));

  assert.deepEqual(paths(error), ['joints[0].kind']);
});

test('parseSceneJson accepts JSON text and reports broken JSON with no path', () => {
  const spec = parseSceneJson(JSON.stringify({ bodies: [ball] }));
  assert.equal(spec.bodies[0]!.id, 'ball');

  const error = catchSceneError(() => parseSceneJson('{ "bodies": ['));
  assert.equal(error.kind, 'syntax');
  assert.deepEqual(paths(error), ['']);
  assert.match(error.message, /not valid JSON/);
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
