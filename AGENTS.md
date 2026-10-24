# AGENTS.md

Newton: a physics visualizer. Natural-language descriptions of physical systems become
time-evolving visualizations showing forces and properties.

Currently a minimal 2D canvas demo: one static platform, one ball on a distance joint.
Rendering and the sim are already separated so the scene source can change without
touching the renderer.

## Commands

```bash
nix develop          # node 24 + tsc (flake.nix)
npm install
npm run build        # tsc -> dist/
npm run dev          # tsc && python3 -m http.server 8080, then open localhost:8080
npm test             # tsc && node --test 'dist/**/*.test.js'
```

Tests are `node:test` + `node:assert/strict`, colocated as `src/**/*.test.ts`.
`npm test` compiles first — a type error fails the test run. Always run it after changes.

## Layout

```
index.html            importmap maps bare "planck" and "zod" to their ESM builds
src/main.ts           bootstrap: canvas, camera, 1/120 step loop, rAF, scene JSON editor
src/scene.json        the document the editor starts from (`npm run dev` serves it)
src/demo.test.ts      guards demo.json: parses, builds, and simulates as intended
src/scene/schema.ts   zod syntax gate: parseScene() / parseSceneJson() -> SceneSpec
src/scene/semantics.ts validateSemantics(): unique ids, joint refs, massless/degenerate shapes
src/scene/world.ts    buildWorld(SceneSpec) -> planck World. Only file constructing planck bodies
src/scene/error.ts    SceneError with per-path issues, listing every problem at once
src/scene/index.ts    buildScene() = parse -> validateSemantics -> buildWorld, plus re-exports
src/render/types.ts   Frame / RenderBody / ShapeGeometry / Camera / Renderer interfaces
src/render/frame.ts   planck World -> Frame. Only file importing planck for reading
src/render/camera.ts  worldToScreen(): metres -> pixels, y flipped. Pure math
src/render/canvas2d.ts CanvasRenderer — only file that touches <canvas>
```

## Conventions

- **Layering is the point.** `frame.ts` flattens everything through planck's
  `DistanceProxy`, so backends never see planck. `canvas2d.ts` never imports planck.
  Keep that true: a new backend means one new file implementing `Renderer`, nothing else.
- Mutable sim state stays in planck; `Frame` is a fresh plain value per step.
  Don't hand out live planck vectors in `Frame`.
- Body identity travels via `body.setUserData(name)` -> `RenderBody.id`. Tests assert on it.
- Units: metres in world space, `Camera.scale` is pixels per metre.
- TS is strict, `NodeNext` module resolution — relative imports need the `.js` extension
  (`./scene.js`), even from `.ts`.
- Compiled output goes to `dist/` and is gitignored; never commit JS next to sources
  (`.gitignore` guards `src/**/*.js`).

## Scene document (`src/scene/`)

A scene is JSON: `{ gravity, bodies[], joints[] }`, SI units, +y up. It travels
`parseScene` (zod, syntax) -> `validateSemantics` (physics) -> `buildWorld` (planck);
`buildScene` runs all three. Any failure throws `SceneError` with every issue listed and
its path (`bodies[1].shape.radius`), so a generator can fix them in one pass.

```jsonc
{
  "gravity": { "x": 0, "y": -9.81 },          // m/s², default -9.81 on y
  "bodies": [
    { "id": "platform", "type": "static",     // ids are unique, non-blank, -> RenderBody.id
      "position": { "x": 0, "y": -10 }, "angle": 0, "density": 1,
      "shape": { "kind": "edge", "a": { "x": -50, "y": 0 }, "b": { "x": 50, "y": 0 } } }
  ],
  "joints": [
    { "kind": "distance", "bodyA": "platform", "bodyB": "ball",
      "localAnchorA": { "x": 0, "y": 0 }, "localAnchorB": { "x": 0, "y": 0 },
      "length": 2, "collideConnected": false }
  ]
}
```

Rules that hold today:

- Shapes: `circle{radius}`, `box{halfWidth,halfHeight}`, `edge{a,b}` — one per body,
  local to the body. Compound bodies become a `shapes` array when there is a need.
- Joint anchors are **local** to their body, so rotating/moving a body keeps the joint
  attached. `length` is optional: omitted means "measure the anchors at build time";
  `0` is a pin. Only `distance` exists; new kinds go in the `kind` union in `schema.ts`.
- Validation at the boundary, never a silent fallback: unknown keys, unknown `kind`,
  missing positions, duplicate ids, dangling body refs, self-joints, dynamic edges
  (zero area -> planck's silent 1 kg default), and zero-length edges (a point nothing
  can collide with) are all rejected by path.
- Every schema addition gets a test in `src/scene/*.test.ts`: parse a small document,
  step the world, assert on the resulting `Frame` or body state.
- The document does not leak into the renderer: anything it expresses renders through
  the existing `Frame`/`Renderer` contract.
- `main.ts` fetches `/src/scene.json` (served verbatim by `npm run dev`, so the textarea
  shows the file) into the right-hand editor. Apply parses the text with
  `buildSceneJson` and swaps the running world; a bad document keeps the old scene on
  screen and prints its `SceneError` paths.

Skipped until needed: forces/constraints beyond distance joints, compound shapes,
LLM call path.
