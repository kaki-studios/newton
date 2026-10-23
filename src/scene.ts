import { Circle, DistanceJoint, Edge, World, type Body } from 'planck';

export interface Scene {
  world: World;
  /** Bodies in creation order; the renderer needs them, ids travel via userData. */
  bodies: Body[];
}

/** The scene the visualizer starts from. Swap in LLM-generated specs later. */
export function createScene(): Scene {
  const world = new World({ gravity: { x: 0, y: -10 } });

  const platform = world.createBody({
    type: 'static',
    position: { x: 0, y: -10 },
    // angle: Math.PI * 0.1,
  });
  platform.setUserData('platform');
  platform.createFixture({ shape: new Edge({ x: -50, y: 0 }, { x: 50, y: 0 }) });

  // ponytail: throwaway demo body so the renderer has something moving; a real
  // scene description replaces this.
  const ball = world.createBody({ type: 'dynamic', position: { x: 10, y: 6 } });
  // ball.setLinearVelocity({x: 1, y: 0});
  const joint = new DistanceJoint({ collideConnected: true}, platform, ball, { x: 1, y: -10}, {x:10, y:6})
  ball.setUserData('ball');
  ball.createFixture(new Circle(0.8), { density: 1 });
  world.createJoint(joint);

  return { world, bodies: [platform, ball] };
}
