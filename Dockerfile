# Etapa 1: compilar
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /relay ./cmd/api

# Etapa 2: correr
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relay /relay
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/relay"]
