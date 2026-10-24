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

  const ball = world.createBody({ type: 'dynamic', position: { x: 10, y: 6 } });
  const ball2 = world.createBody({ type: 'dynamic', position: { x: 10, y: 3 } });

  const joint = new DistanceJoint({ collideConnected: true}, platform, ball, { x: 1, y: 10}, {x:10, y:6})
  const joint2 = new DistanceJoint({ collideConnected: true}, ball, ball2, {x: 10, y: 6}, {x:10, y:3});

  ball.setUserData('ball');
  ball2.setUserData('ball2');

  ball.createFixture(new Circle(0.8), { density: 1 });
  ball2.createFixture(new Circle(0.4), { density: 0.5 });
  world.createJoint(joint);
  world.createJoint(joint2);

  return { world, bodies: [platform, ball, ball2] };
}
