FROM golang:1.14 as builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 go build -o consul-envoy-xds-server .


FROM alpine:3.11

RUN apk add --no-cache ca-certificates && update-ca-certificates
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
ENV SSL_CERT_DIR=/etc/ssl/certs

COPY --from=builder /app/consul-envoy-xds-server /consul-envoy-xds-server
CMD ["/consul-envoy-xds-server"]
