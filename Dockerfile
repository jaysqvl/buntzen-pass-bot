# syntax=docker/dockerfile:1.7

FROM golang:1.27.0-bookworm AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/buntzen ./cmd/buntzen

FROM mcr.microsoft.com/playwright/python:v1.62.0-noble

ENV APPDATA_DIR=/appdata \
    BUNTZEN_LISTEN=:8080 \
    BUNTZEN_PYTHON=/usr/bin/python \
    BUNTZEN_ACTIONS_MODULE=buntzen_actions \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app
COPY actions ./actions
RUN python -m pip install --no-cache-dir ./actions \
    && rm -rf /app/actions \
    && mkdir -p /appdata \
    && chown -R pwuser:pwuser /app /appdata
COPY --from=go-build /out/buntzen /usr/local/bin/buntzen

USER pwuser
EXPOSE 8080
VOLUME ["/appdata"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=3).read()"]

ENTRYPOINT ["/usr/local/bin/buntzen"]
CMD ["serve"]
