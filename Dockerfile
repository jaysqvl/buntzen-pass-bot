# syntax=docker/dockerfile:1.7

FROM golang:1.27.1-bookworm AS go-build
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
COPY actions/requirements.lock /tmp/buntzen-actions-requirements.txt
RUN python -m pip install --no-cache-dir --require-hashes --requirement /tmp/buntzen-actions-requirements.txt \
    && python -m pip check \
    && python -m pip uninstall --yes virtualenv \
    && python -m pip uninstall --yes pip \
    && rm /tmp/buntzen-actions-requirements.txt \
    && mkdir -p /appdata \
    && chown -R pwuser:pwuser /app /appdata
COPY actions/src/buntzen_actions /usr/local/lib/python3.12/dist-packages/buntzen_actions
COPY --from=go-build /out/buntzen /usr/local/bin/buntzen

USER pwuser
EXPOSE 8080
VOLUME ["/appdata"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=3).read()"]

ENTRYPOINT ["/usr/local/bin/buntzen"]
CMD ["serve"]
