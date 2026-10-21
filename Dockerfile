# Build stage
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=""
ARG DATE=""
RUN BI=github.com/sebasBetancourt/relay/internal/buildinfo && \
    LDFLAGS="-s -w -X $BI.Version=$VERSION -X $BI.Commit=$COMMIT -X $BI.Date=$DATE" && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/relay ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$LDFLAGS" -o /out/relayctl ./cmd/relayctl

# Run stage: no shell, no package manager, non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/relay /out/relayctl /usr/local/bin/
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/relay"]
