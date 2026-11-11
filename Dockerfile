# Targets: migrations (Liquibase + changelog), collector (batch job), web (default, last).
FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /out/dashboard ./cmd/dashboard \
 && CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /out/collector ./cmd/collector

# Liquibase is the only schema owner. The changelog is baked in for Kubernetes Jobs;
# Compose mounts ./db/changelog over it for development.
FROM liquibase:4.33.0 AS migrations
COPY --chown=liquibase:liquibase db/changelog /liquibase/changelog
ENV LIQUIBASE_SEARCH_PATH=/liquibase/changelog \
    LIQUIBASE_COMMAND_CHANGELOG_FILE=db.changelog-master.xml \
    LIQUIBASE_ANALYTICS_ENABLED=false
CMD ["update"]

# Run-to-completion collector. Git is required for diff statistics; mirrors live in /data.
FROM debian:bookworm-20261005-slim AS collector
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git tzdata && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --create-home collector && mkdir -p /data/mirrors && chown -R collector:collector /data
COPY --from=build /out/collector /usr/local/bin/collector
USER 10001:10001
WORKDIR /data
ENV GIT_MIRROR_DIR=/data/mirrors
VOLUME ["/data"]
ENTRYPOINT ["collector"]
CMD ["--mode", "scheduled"]

FROM debian:bookworm-20261005-slim AS web
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/* && useradd --uid 10001 --create-home dashboard
COPY --from=build /out/dashboard /usr/local/bin/dashboard
USER 10001:10001
WORKDIR /home/dashboard
ENV LISTEN_ADDR=0.0.0.0:8080
EXPOSE 8080 9090
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["dashboard", "healthcheck"]
ENTRYPOINT ["dashboard"]
CMD ["serve"]
