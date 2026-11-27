# The store's image: the mock commerce REST API, a Kubernetes component on the platform.
# golang 1.27.4 and distroless static-debian13:nonroot, digests checked on their registries
# 2026-11-26 (docker buildx imagetools inspect).
FROM golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/store ./cmd/store

FROM build AS test
CMD ["sh", "-c", "go vet ./... && test -z \"$(gofmt -l .)\" && go test -race -count=1 ./..."]

# Last, so a plain `docker build` produces the image the platform deploys.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5 AS store
COPY --from=build /out/store /store
EXPOSE 8080
ENTRYPOINT ["/store"]
