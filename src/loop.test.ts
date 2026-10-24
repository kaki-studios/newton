import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createAccumulator } from './loop.js';

/** Simulated seconds after one second of wall-clock frames at the given render rate. */
function simulated(dt: number, elapsedPerFrame: number, frames: number): number {
  const stepsDue = createAccumulator(dt);
  let steps = 0;
  for (let i = 0; i < frames; i++) steps += stepsDue(elapsedPerFrame);
  return steps * dt;
}

test('a smaller dt does not slow the simulation down', () => {
  for (const dt of [1 / 60, 1 / 120, 1 / 240]) {
    const sim = simulated(dt, 1 / 60, 60);
    assert.ok(
      Math.abs(sim - 1) <= dt,
      `dt=${dt} simulated ${sim} s of physics per 1 s of wall time; expected 1 s within one step`,
    );
  }
});

test('a slow frame catches up with several steps, not one', () => {
  const stepsDue = createAccumulator(1 / 120);
  assert.equal(stepsDue(1 / 30), 4); // a 30 Hz frame is four 120 Hz steps
  assert.equal(stepsDue(0), 0);
});

test('a long stall is capped instead of replaying a burst of physics', () => {
  const stepsDue = createAccumulator(1 / 60, 0.1);
  assert.equal(stepsDue(5), 6); // 0.1 s cap -> 6 steps, not 300
});

test('the accumulator never goes backwards on bad deltas', () => {
  const stepsDue = createAccumulator(1 / 60);
  assert.equal(stepsDue(-1), 0);
  assert.equal(stepsDue(Number.NaN), 0);
  assert.equal(stepsDue(1 / 60), 1); // still in sync afterwards
});

test('the first frame is not lost to a rounding error', () => {
  const stepsDue = createAccumulator(1 / 60);
  assert.equal(stepsDue(1 / 60), 1);
});
