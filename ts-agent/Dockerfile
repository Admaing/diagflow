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
# Custom provider catalog (modelverse). The deployment swaps in a ConfigMap;
# this keeps the DeepSeek-only default working when none is mounted.
ENV TS_AGENT_MODELS_JSON=/app/models.json
COPY --from=build /src/node_modules ./node_modules
COPY --from=build /src/dist ./dist
COPY --from=build /src/package.json ./package.json
COPY ts-agent/models.json ./models.json
COPY data/strategies ./data/strategies
USER node
EXPOSE 8099
# Kubernetes runs the container with a read-only root filesystem, so HOME and
# the Pi scratch dirs must live under the /tmp emptyDir. Create them defensively
# (a failure here must not stop startup; the agent creates its own dirs too) and
# exec node so it becomes PID 1 and receives SIGTERM directly.
CMD ["sh", "-c", "mkdir -p \"${HOME:-/tmp}\" \"${TS_AGENT_AGENT_DIR:-/tmp/pi-agent}\" \"${TS_AGENT_WORKSPACE:-/tmp/pi-workspace}\" 2>/dev/null || true; exec node dist/server.js"]
