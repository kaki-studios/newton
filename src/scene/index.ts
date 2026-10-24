import { parseScene, parseSceneJson } from './schema.js';
import { validateSemantics } from './semantics.js';
import { buildWorld, type Scene } from './world.js';

export { SceneError, type SceneIssue } from './error.js';
export { parseScene, parseSceneJson, sceneSpecSchema } from './schema.js';
export type { BodySpec, JointSpec, SceneSpec, ShapeSpec, Vec2 } from './schema.js';
export { validateSemantics } from './semantics.js';
export { buildWorld, type Scene } from './world.js';

// Whole pipeline for an already-parsed document:
// parseScene (syntax) -> validateSemantics (physics) -> buildWorld (planck).
//
// Every step throws `SceneError` for bad input, so callers only need one catch.

export function buildScene(doc: unknown): Scene {
  return buildWorld(validateSemantics(parseScene(doc)));
}

/** Same pipeline for raw JSON text — what the editor textarea holds. */
export function buildSceneJson(text: string): Scene {
  return buildWorld(validateSemantics(parseSceneJson(text)));
}
