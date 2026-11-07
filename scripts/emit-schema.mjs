// Emits the scene JSON Schema from the TypeScript validator, so the prompt the model sees and
// the schema the browser enforces cannot drift apart.
//
// The artifact is committed at server/internal/prompt/scene-schema.json and embedded into the
// Go binary; `go test ./internal/prompt` fails when regenerating produces different bytes.
//
// Why io:'input': with the default output mode, defaulted fields (angle, density, gravity,
// joints, linearVelocity) land in `required` and the model is told to emit fields it does not
// need. Input mode requires only what a caller must actually write: bodies, and
// id/type/position/shape on each body.
//
// Run from the repo root: npm run schema
import { mkdir, writeFile } from 'node:fs/promises';
import { sceneSpecSchema } from '../dist/scene/schema.js';
import { z } from 'zod';

const schema = z.toJSONSchema(sceneSpecSchema, { io: 'input' });
const out = `${JSON.stringify(schema, null, 2)}\n`;
const target = 'server/internal/prompt/scene-schema.json';
await mkdir('server/internal/prompt', { recursive: true });
await writeFile(target, out);
console.log(`wrote ${target} (${out.length} bytes)`);
