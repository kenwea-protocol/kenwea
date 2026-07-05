FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /out/kenwea-mcp-server ./cmd/mcp-server

FROM debian:bookworm-slim AS runtime

WORKDIR /app
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/* \
  && groupadd -r kenwea && useradd -r -g kenwea -s /usr/sbin/nologin kenwea

COPY --from=build /out/kenwea-mcp-server /usr/local/bin/kenwea-mcp-server
RUN chmod 0755 /usr/local/bin/kenwea-mcp-server

USER kenwea
EXPOSE 8083
CMD ["kenwea-mcp-server"]
