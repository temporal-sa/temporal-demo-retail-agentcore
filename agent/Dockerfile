# Ziggy's Worker image. AgentCore runs ARM64 containers only, so this builds for whichever
# platform buildx asks for (no --platform pins); the platform builds it as linux/arm64.
# Versions checked on 2026-11-26: python 3.15.2 (Docker Hub), uv 0.13.10 (GitHub releases).
FROM ghcr.io/astral-sh/uv:0.13.10@sha256:a3ea1357342c1a4bf9a0c53a3deb1b55379aefa037338d8113d5771efa01774f AS uv

FROM python:3.15.2-slim-trixie@sha256:63bb298d2b7b88f176f51b5189f54ce6535c997682ea70885d2132f5d473a8a5 AS base
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PROJECT_ENVIRONMENT=/venv PYTHONUNBUFFERED=1
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN uv sync --locked --no-default-groups --no-install-project

FROM base AS test
RUN uv sync --locked --group test --no-install-project
COPY . .
CMD ["/venv/bin/python", "-m", "pytest", "-q"]

FROM python:3.15.2-slim-trixie@sha256:63bb298d2b7b88f176f51b5189f54ce6535c997682ea70885d2132f5d473a8a5 AS runtime
COPY --from=base /venv /venv
WORKDIR /app
COPY agentcore_worker.py ./
COPY crew ./crew
ENV PATH=/venv/bin:$PATH PYTHONUNBUFFERED=1
USER 65532:65532
EXPOSE 8080
CMD ["python", "agentcore_worker.py"]
