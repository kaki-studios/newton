# MVP
- [ ] Show forces
    - [X] Joints
    - [X] Gravity
    - [X] Normal forces (The contact post-solve callback gives impulses; divide by the time step)
    - [ ] Applied forces
    - [ ] Friction forces (The contact post-solve callback gives impulses; divide by the time step)
- [ ] Implement springs (apply spring force F=-kx yourself, with leapfrog or velocity Verlet)
- [ ] AI editing
    - [X] JSON->scene
    - [X] JSON editor with hot-reloading
    - [ ] AI writes JSON
- [ ] Show energy
    - [X] Kinetic (1/2 mv^2 + 1/2I omega^2)
    - [X] Potential (mgh)
    - [ ] Spring force (1/2 kx^2)

# Bugs
- [ ] Tabs don't work in the JSON edotor

