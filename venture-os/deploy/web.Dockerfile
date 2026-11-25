# SvelteKit Command Center (adapter-node). Requires web/ (build step 6) with:
#   - `npm run build` producing ./build via @sveltejs/adapter-node
#   - a GET /healthz route returning 200
FROM node:22-alpine AS build
WORKDIR /app
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY web/ ./
RUN npm run build && npm prune --omit=dev

FROM node:22-alpine
ENV NODE_ENV=production
WORKDIR /app
RUN addgroup -S -g 10001 venture && adduser -S -u 10001 -G venture venture
COPY --from=build --chown=venture:venture /app/build ./build
COPY --from=build --chown=venture:venture /app/node_modules ./node_modules
COPY --from=build --chown=venture:venture /app/package.json ./package.json
USER venture
EXPOSE 3000
CMD ["node", "build"]
