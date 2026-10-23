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
index.html            importmap maps bare "planck" -> /node_modules/planck/dist/planck.mjs
src/main.ts           bootstrap: canvas, camera, 1/60 step loop, rAF
src/scene.ts          createScene() — the only place bodies/joints are built today
src/render/types.ts   Frame / RenderBody / ShapeGeometry / Camera / Renderer interfaces
src/render/frame.ts   planck World -> Frame. Only file (besides scene.ts) importing planck types
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

## Planned: JSON scene schema (not implemented yet)

Goal: parse a JSON document describing bodies, constraints, and forces, then build the
`World` from it instead of hardcoding in `scene.ts`.

Constraints on the design when you get there:

- The schema feeds `createScene()`; it does not leak into the renderer. Anything the JSON
  can express must render through the existing `Frame`/`Renderer` contract.
- Validate at the parse boundary — unknown shape kinds, missing positions, dangling
  joint body references must produce a specific error naming the offending field/path,
  never a silent fallback or a planck crash later.
- Keep `scene.ts` as the single translation point (JSON -> planck). Don't scatter
  planck construction across modules.
- Reserve the force/constraint vocabulary now but implement it when there is a concrete
  need — no speculative field surface.
- Every schema addition gets a test: parse a small document, step the world, assert on
  the resulting `Frame` or body state.

Skipped until needed: JSON Schema validation library (zod), LLM call path.
