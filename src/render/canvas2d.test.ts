import { test } from 'node:test';
import assert from 'node:assert/strict';
import { CanvasRenderer } from './canvas2d.js';
import type { Energy, Frame } from './types.js';

const DT = 0;
const CAMERA = { center: { x: 0, y: 0 }, scale: 10 };

/** A 2d context stand-in: every call is a no-op except text, which is recorded. */
function stubCanvas(): { canvas: HTMLCanvasElement; texts: string[] } {
  const texts: string[] = [];
  const ctx = new Proxy(
    {},
    {
      get: (_target, key) => (key === 'fillText' ? (text: string) => texts.push(text) : () => {}),
      set: () => true,
    },
  );
  return { canvas: { getContext: () => ctx } as unknown as HTMLCanvasElement, texts };
}

function frame(energy: Energy): Frame {
  return { bodies: [], joints: [], energy };
}

test('the energy chart prints kinetic, potential and total for the current step', () => {
  const { canvas, texts } = stubCanvas();
  new CanvasRenderer(canvas).draw(frame({ kinetic: 2, potential: 1 }), CAMERA);

  assert.deepEqual(texts, ['total 3.0 J', 'kinetic 2.0 J', 'potential 1.0 J']);
});

test('energy history is bounded, so a long session does not grow without limit', () => {
  const { canvas, texts } = stubCanvas();
  const renderer = new CanvasRenderer(canvas);

  for (let i = 0; i < 1000; i++) renderer.draw(frame({ kinetic: i, potential: 0 }), CAMERA);

  const history = (renderer as unknown as { energyHistory: Energy[] }).energyHistory;
  assert.equal(history.length, 600);
  // Oldest samples are dropped, newest kept.
  assert.deepEqual(history.at(-1), { kinetic: 999, potential: 0 });
  assert.deepEqual(history[0], { kinetic: 400, potential: 0 });
  assert.equal(texts.at(-3), 'total 999.0 J');
});
