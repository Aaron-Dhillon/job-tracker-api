# The builder must be >= go.mod's directive. golang-migrate v4.20.1 requires
# go 1.25.11, which sets the floor for the whole module -- still inside the
# PRD's "Go 1.22+". GOTOOLCHAIN=local keeps the build deterministic: it fails
# loudly on a too-old image instead of silently downloading another toolchain.
FROM golang:1.26-bookworm AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# Migrations are embedded in the binary, so nothing else needs copying.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]
