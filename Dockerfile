# check=skip=InvalidDefaultArgInFrom
# The check above is skipped because GO_VERSION has no default on purpose: it
# comes from the `go` directive in go.mod, so there is exactly one place to bump
# the toolchain. Build via hack/docker-build.sh, or pass it yourself:
#   docker build --build-arg "GO_VERSION=$(hack/go-version.sh)" .
ARG GO_VERSION

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS builder

# TARGETOS/TARGETARCH are global BuildKit args and only expand once re-declared
# inside the stage; without these the build falls back to the builder's native
# platform instead of cross-compiling.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /build

COPY pkg /build/pkg

COPY main.go /build/
COPY go.mod /build/
COPY go.sum /build/

RUN \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /build/kairos . && \
    chmod +x kairos

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=builder /build/kairos /usr/local/bin/kairos

CMD ["/usr/local/bin/kairos"]
