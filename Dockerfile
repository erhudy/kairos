FROM golang:1.21.1 AS builder

WORKDIR /build

COPY cmd /build/cmd
COPY kairostest /build/kairostest
COPY pkg /build/pkg

COPY go.mod /build/
COPY go.sum /build/

RUN \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /build/kairos cmd/main.go && \
    chmod +x kairos

FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=builder /build/kairos /usr/local/bin/kairos

CMD ["/usr/local/bin/kairos"]