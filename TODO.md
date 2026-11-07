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
        - [X] Chat tab + `ChatBackend` seam
        - [X] Go backend: static serving, SQLite, migrations, health
        - [X] Auth: GitHub/Google OAuth, opaque sessions, CSRF, ban switch
        - [X] Quota: reserve/settle, daily + per-conversation + global cost caps, in-flight index
        - [X] `POST /api/chat` streaming SSE with a fake provider
        - [X] Prompt assembly from the zod-generated schema artifact
        - [ ] `src/ai/{protocol,sse,jsonblock,client,glue}.ts`: stream -> extract -> validate -> apply -> repair
        - [ ] Flip `PROVIDER=openai` and `CHAT_ENABLED=true` once that client exists
- [ ] Show energy
    - [X] Kinetic (1/2 mv^2 + 1/2I omega^2)
    - [X] Potential (mgh)
    - [ ] Spring force (1/2 kx^2)
- [ ] Share/export systems
- [ ] Store JSON in localStorage

# Bugs
- [ ] Tab key inserts a tab character in the JSON editor (Enter-on-tab got fixed by the tablist,
      which uses arrow keys as ARIA wants)

