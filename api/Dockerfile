# The API's image, plus the local invoker that stands in for Temporal Cloud under Compose.
# golang 1.27.4 and distroless static-debian13:nonroot, digests checked on their registries
# 2026-11-26 (docker buildx imagetools inspect).
FROM golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/local-invoker ./cmd/local-invoker

FROM build AS test
CMD ["sh", "-c", "go vet ./... && test -z \"$(gofmt -l .)\" && go test -race -count=1 ./..."]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS local-invoker
COPY --from=build /out/local-invoker /local-invoker
ENTRYPOINT ["/local-invoker"]

# Last, so a plain `docker build` produces the API image the platform deploys.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS api
COPY --from=build /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
