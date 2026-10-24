/**
 * Fixed-timestep accumulator. A render loop calls this with the wall-clock time since
 * the last frame and gets back how many fixed `dt` steps to simulate, so simulated
 * time tracks real time whatever `dt` the caller chose.
 *
 * `maxSeconds` caps how much time one flush can bank, so a backgrounded tab slows the
 * sim down instead of replaying minutes of physics in a single frame.
 */
export function createAccumulator(dt: number, maxSeconds = 0.1): (elapsedSeconds: number) => number {
  if (!(dt > 0)) throw new Error(`accumulator needs a positive dt, got ${dt}`);
  let pending = 0;

  return function stepsDue(elapsedSeconds: number): number {
    // Negative or non-finite deltas (clock skew, first frame) must not rewind the accumulator.
    const elapsed = Number.isFinite(elapsedSeconds) && elapsedSeconds > 0 ? elapsedSeconds : 0;
    pending = Math.min(pending + elapsed, maxSeconds);

    // Half a step of slack: plain float division otherwise loses a step to 0.9999999.
    const steps = Math.floor(pending / dt + 1e-9);
    pending -= steps * dt;
    if (pending < 0) pending = 0; // the slack above may overdraw by < 1e-9 s
    return steps;
  };
}
