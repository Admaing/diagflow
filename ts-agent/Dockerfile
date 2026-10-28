# Build from the repository root:
#   docker build -f ts-agent/Dockerfile -t diagflow-ts-agent .
FROM node:22-bookworm-slim AS build
WORKDIR /src
COPY ts-agent/package.json ts-agent/package-lock.json ./
RUN npm ci
COPY ts-agent/tsconfig.json ./
COPY ts-agent/src ./src
RUN npm run build && npm prune --omit=dev

FROM node:22-bookworm-slim
WORKDIR /app
ENV NODE_ENV=production
ENV TS_AGENT_STRATEGIES_DIR=/app/data/strategies
# The runtime needs writable scratch dirs for Pi sessions/agent state.
ENV TS_AGENT_AGENT_DIR=/tmp/diagflow-ts/agent
ENV TS_AGENT_WORKSPACE=/tmp/diagflow-ts/workspace
COPY --from=build /src/node_modules ./node_modules
COPY --from=build /src/dist ./dist
COPY --from=build /src/package.json ./package.json
COPY data/strategies ./data/strategies
USER node
EXPOSE 8099
CMD ["node", "dist/server.js"]
