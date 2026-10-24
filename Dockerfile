# syntax=docker/dockerfile:1

# --- build: compile the TypeScript into dist/ ---
FROM node:24-alpine AS build
WORKDIR /app
# tsconfig.json has "types": ["node"], so @types/node is needed here too: no --omit=dev.
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY index.html ./
COPY src/ ./src/
RUN npm run build

# --- serve: static files only, the browser resolves the importmap itself ---
FROM nginx:stable-alpine
WORKDIR /usr/share/nginx/html

COPY Dockerfile.nginx /etc/nginx/conf.d/default.conf
COPY --from=build /app/index.html ./
COPY --from=build /app/dist/ ./dist/
# index.html's importmap points at these two files: they are the app's dependencies.
COPY --from=build /app/node_modules/planck/dist/planck.mjs ./node_modules/planck/dist/
COPY --from=build /app/node_modules/zod/ ./node_modules/zod/

EXPOSE 80
