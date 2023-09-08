FROM golang:1.21.1 AS builder

WORKDIR /build

COPY *.go /build/
COPY kairostest /build/kairostest
COPY go.mod /build/
COPY go.sum /build/

RUN go build -o kairos *.go

FROM istio/distroless:latest AS runtime

COPY --from=builder /build/kairos /kairos

CMD ["/kairos"]