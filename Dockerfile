FROM ghcr.io/astral-sh/uv:0.11.21 AS uv

FROM mcr.microsoft.com/playwright/python:v1.60.0-noble

WORKDIR /app

ENV APPDATA_DIR=/appdata \
    PYTHONUNBUFFERED=1 \
    UV_LINK_MODE=copy

COPY --from=uv /uv /usr/local/bin/uv
COPY pyproject.toml uv.lock ./
RUN uv sync --locked

COPY . .
RUN mkdir -p /appdata

EXPOSE 8080

CMD ["uv", "run", "uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8080"]
