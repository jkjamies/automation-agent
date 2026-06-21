# Container image for the automation-agent (Python) service.
#
# Only cmd/agent is run in the image — the cmd/playground dev tool is never part of a
# deployed artifact. Uses uv for fast, reproducible installs.

FROM python:3.13-slim AS base
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

WORKDIR /app

# Install dependencies first (better layer caching).
COPY pyproject.toml uv.lock* README.md ./
COPY automation_agent ./automation_agent
RUN uv sync --no-dev --frozen 2>/dev/null || uv sync --no-dev

# Application code (entrypoint is composition-only).
COPY cmd ./cmd

EXPOSE 8080
ENTRYPOINT ["uv", "run", "python", "cmd/agent/main.py"]
