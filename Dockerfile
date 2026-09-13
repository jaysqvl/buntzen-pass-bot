# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG LAKE_PASS_VERSION=dev
ARG LAKE_PASS_REVISION=""
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/jaysqvl/lake-pass-bot/internal/buildinfo.Version=${LAKE_PASS_VERSION} -X github.com/jaysqvl/lake-pass-bot/internal/buildinfo.Revision=${LAKE_PASS_REVISION}" \
    -o /out/lake-pass-bot ./cmd/lake-pass-bot

FROM mcr.microsoft.com/playwright/python:v1.62.0-noble@sha256:aa81288e738725378becba5b3e06cb0f3a7f012a610e87e8d767a090ea3f740d

ENV APPDATA_DIR=/appdata \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /app
COPY actions/requirements.lock /tmp/lake-pass-actions-requirements.txt
RUN LC_ALL=C apt-get --simulate purge gstreamer1.0-plugins-bad libgstreamer-plugins-bad1.0-0 > /tmp/lake-pass-purge-plan \
    && cat /tmp/lake-pass-purge-plan \
    && awk '/^Inst / { exit 1 } /^(Remv|Purg) / { if ($2 != "gstreamer1.0-plugins-bad" && $2 != "libgstreamer-plugins-bad1.0-0") exit 1 }' /tmp/lake-pass-purge-plan \
    && apt-get --yes --no-auto-remove purge gstreamer1.0-plugins-bad libgstreamer-plugins-bad1.0-0 \
    && python -m pip install --no-cache-dir --only-binary=:all: --require-hashes --requirement /tmp/lake-pass-actions-requirements.txt \
    && python -m pip check \
    && python -m pip uninstall --yes virtualenv msgpack setuptools \
    && python -m pip uninstall --yes pip \
    && rm /tmp/lake-pass-actions-requirements.txt /tmp/lake-pass-purge-plan \
    && rm -rf /root/.cache /home/pwuser/.cache /var/lib/apt/lists/* \
    && mkdir -p /appdata \
    && chown -R pwuser:pwuser /app /appdata
COPY actions/src/lake_pass_actions /usr/local/lib/python3.12/dist-packages/lake_pass_actions
COPY actions/src/buntzen_actions /usr/local/lib/python3.12/dist-packages/buntzen_actions
COPY --from=go-build /out/lake-pass-bot /usr/local/bin/lake-pass-bot
# Preserve existing operator commands during the rename.
RUN ln -s lake-pass-bot /usr/local/bin/buntzen

USER pwuser
EXPOSE 8080
VOLUME ["/appdata"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/healthz', timeout=3).read()"]

ENTRYPOINT ["/usr/local/bin/lake-pass-bot"]
CMD ["serve"]
