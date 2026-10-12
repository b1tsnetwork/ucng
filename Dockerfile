# UCNG
#   docker build -t ucng .                                  # nur Dateien als Speicher
#   docker build --build-arg TAGS="sqlite mysql" -t ucng .  # zusätzlich SQLite/MySQL
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TAGS=""
ARG VERSION=dev
# Adresse des Quellcodes dieser Fassung (AGPL), z. B. https://github.com/<owner>/ucng/tree/<commit>
ARG SOURCE_URL=""
ARG TARGETOS
ARG TARGETARCH
# Neuere Treiber-Versionen dürfen eine neuere Go-Version nachladen.
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY . .
RUN for t in $TAGS; do \
      case "$t" in \
        sqlite) go get modernc.org/sqlite ;; \
        mysql)  go get github.com/go-sql-driver/mysql ;; \
      esac; \
    done && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -tags "$TAGS" -trimpath -ldflags="-s -w -X main.version=$VERSION -X main.sourceURL=$SOURCE_URL" -o /ucng .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && \
    adduser -D -H ucng && mkdir /data && chown ucng /data
USER ucng
COPY --from=build /ucng /usr/local/bin/ucng
COPY --from=build /src/LICENSE /src/web/flags/LICENSE.txt /usr/share/licenses/ucng/
ENV ADDR=:8080 DATA_DIR=/data BACKUP_DIR=/data/backups
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["ucng"]
