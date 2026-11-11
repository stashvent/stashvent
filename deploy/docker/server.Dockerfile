# Builds the stashvent-server image.
#   docker build -f deploy/docker/server.Dockerfile --build-arg VERSION=v0.1.0 .

FROM golang:1.27.4-alpine AS build
WORKDIR /src
# For the server build and for go-licenses alike, so the licenses collected are
# those of exactly the packages the binary compiles.
ENV CGO_ENABLED=0

# Download dependencies first so they're cached separately from source changes.
COPY go.mod go.sum* ./
RUN go mod download

# go-licenses, from the dev tools module, collects the dependencies' licenses
# the image must carry. Built before the source is copied, so it stays cached.
COPY tools/go.mod tools/go.sum ./tools/
RUN go build -modfile=tools/go.mod -o /tools/go-licenses github.com/google/go-licenses/v2

COPY apps ./apps
COPY apikey ./apikey
COPY internal ./internal

ARG VERSION=dev
RUN go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/stashvent-server ./apps/stashvent-server
RUN /tools/go-licenses save ./apps/stashvent-server --ignore github.com/stashvent/stashvent \
    --save_path /out/third_party_licenses

FROM gcr.io/distroless/static-debian13:nonroot
# GHCR links each pushed image to this repo, whichever repo first created the package.
LABEL org.opencontainers.image.source=https://github.com/stashvent/stashvent
COPY LICENSE NOTICE /
COPY --from=build /out/third_party_licenses /third_party_licenses
COPY --from=build /out/stashvent-server /stashvent-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/stashvent-server"]
