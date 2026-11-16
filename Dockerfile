# golem's images: one Dockerfile, the binary chosen by BINARY
# (golem-gateway or golem-control). Static, on distroless, non-root.
#
#   docker build --build-arg BINARY=golem-gateway -t golem-gateway .
# Behind Docker Hub's rate limit: --build-arg GO_IMAGE=public.ecr.aws/docker/library/golang
ARG GO_VERSION=1.27.3

ARG GO_IMAGE=golang
FROM ${GO_IMAGE}:${GO_VERSION} AS build
ARG BINARY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN test -n "$BINARY" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/app ./cmd/${BINARY}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
