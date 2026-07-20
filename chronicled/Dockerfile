# syntax=docker/dockerfile:1
# chronicled — multi-stage build to a static binary in a distroless image.
#
# Build from the REPOSITORY ROOT, not this directory: the chronicled module
# depends on its sibling modules via replace directives.
#
#   docker build -f chronicled/Dockerfile -t chronicled .
#
# The final image is distroless/static: no shell, no libc, no package
# manager — the binary is the whole attack surface, and CGO_ENABLED=0 is
# what makes that possible.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Module files first, so dependency downloads cache across code changes.
COPY go.mod ./
COPY pgstore/go.mod pgstore/go.sum ./pgstore/
COPY chronicled/go.mod chronicled/go.sum ./chronicled/
RUN cd chronicled && go mod download

COPY . .
RUN cd chronicled && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o /chronicled ./cmd/chronicled

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /chronicled /chronicled
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/chronicled"]
