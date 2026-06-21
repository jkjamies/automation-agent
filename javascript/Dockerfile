# Container image for the automation-agent (TypeScript) service.
#
# Only cmd/agent is run in the image — the cmd/playground dev tool is never part of a
# deployed artifact. The app runs directly from TypeScript via tsx (no build step).

FROM node:22-slim AS base

WORKDIR /app

# Install dependencies first (better layer caching).
COPY package.json package-lock.json* ./
RUN npm ci --omit=dev 2>/dev/null || npm install --omit=dev

# Application code (entrypoint is composition-only).
COPY tsconfig.json ./
COPY src ./src
COPY cmd ./cmd

EXPOSE 8080
ENTRYPOINT ["npx", "tsx", "cmd/agent/main.ts"]
