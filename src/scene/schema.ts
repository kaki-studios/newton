import { z } from "zod";
import { SceneError, type SceneIssue } from "./error.js";

/**
 * The scene document schema. SI units everywhere — metres, seconds, kilograms, radians —
 * and +y is up, matching planck and the renderer.
 *
 * Parsing only checks the *shape* of the data (types, required fields, unknown keys).
 * Physical rules (unique ids, dangling joint references, massless bodies) live in
 * `semantics.ts`, so this file stays a plain data description. New shape and joint kinds
 * are additive: both are discriminated unions keyed on `kind`.
 *
 * Body ids are the scene's identity: they become `body.setUserData(id)` and reach the
 * renderer as `RenderBody.id`, so they are required, trimmed, and unique (see semantics).
 */

/** `y` is up. Metres in world space; metres in a body's local frame for joint anchors. */
const vec2 = z.strictObject({ x: z.number(), y: z.number() });

/**
 * Geometry in the body's local frame, i.e. before the body's position/angle are applied.
 * One shape per body for now; compound bodies become a `shapes` array when there is a need.
 */
const shape = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("circle"), radius: z.number().positive() }),
  z.strictObject({
    kind: z.literal("box"),
    halfWidth: z.number().positive(),
    halfHeight: z.number().positive(),
  }),
  z.strictObject({ kind: z.literal("edge"), a: vec2, b: vec2 }),
]);

const body = z.strictObject({
  id: z.string().trim().min(1),
  type: z.enum(["static", "kinematic", "dynamic"]),
  /** Body origin in world metres. */
  position: vec2,
  /** Radians, counter-clockwise. */
  angle: z.number().default(0),
  /** kg/m². Only dynamic bodies gain mass from it. */
  density: z.number().positive().default(1),
  shape,
  linearVelocity: vec2.default(() => ({ x: 0, y: 0 })),
});

/**
 * A rigid rod between two anchor points. Anchors are local to their body, so moving or
 * rotating a body keeps the joint attached where the author meant. `length` defaults to
 * the world distance between the anchors at build time — a pin when they coincide.
 */
const distanceJoint = z.strictObject({
  kind: z.literal("distance"),
  bodyA: z.string().trim().min(1),
  bodyB: z.string().trim().min(1),
  localAnchorA: vec2.default(() => ({ x: 0, y: 0 })),
  localAnchorB: vec2.default(() => ({ x: 0, y: 0 })),
  /** Rod length in metres; 0 is a pin. Omit to measure the anchors instead. */
  length: z.number().nonnegative().optional(),
  /** Usually false, so a rod between two touching bodies still collides. */
  collideConnected: z.boolean().default(false),
});

const joint = z.discriminatedUnion("kind", [distanceJoint]);

export const sceneSpecSchema = z.strictObject({
  /** m/s². */
  gravity: vec2.default(() => ({ x: 0, y: -9.81 })),
  bodies: z.array(body).min(1),
  joints: z.array(joint).default(() => []),
});

export type Vec2 = z.infer<typeof vec2>;
export type ShapeSpec = z.infer<typeof shape>;
export type BodySpec = z.infer<typeof body>;
export type DistanceJointSpec = z.infer<typeof distanceJoint>;
export type JointSpec = z.infer<typeof joint>;
/** A document that passed the syntax gate: same shape, every default filled in. */
export type SceneSpec = z.infer<typeof sceneSpecSchema>;

/**
 * Syntax gate. Takes an already-parsed JSON value — a `{gravity, bodies, joints}`
 * document — and either returns a complete `SceneSpec` or throws a `SceneError`
 * listing every problem with the path that caused it.
 */
export function parseScene(doc: unknown): SceneSpec {
  const result = sceneSpecSchema.safeParse(doc);
  if (!result.success) {
    throw new SceneError("syntax", result.error.issues.map(toIssue));
  }
  return result.data;
}

/** Same as `parseScene`, for raw JSON text — the form a model or a file hands you. */
export function parseSceneJson(text: string): SceneSpec {
  let doc: unknown;
  try {
    doc = JSON.parse(text);
  } catch (error) {
    const reason = error instanceof Error ? error.message : String(error);
    throw new SceneError("syntax", [
      { path: "", message: `not valid JSON: ${reason}` },
    ]);
  }
  return parseScene(doc);
}

function toIssue(issue: z.core.$ZodIssue): SceneIssue {
  return { path: formatPath(issue.path), message: issue.message };
}

/** `['bodies', 0, 'position', 'y']` -> `bodies[0].position.y`, so the path is clickable. */
function formatPath(path: readonly PropertyKey[]): string {
  return path.reduce<string>(
    (out, key) =>
      typeof key === "number"
        ? `${out}[${key}]`
        : out
          ? `${out}.${String(key)}`
          : String(key),
    "",
  );
}
