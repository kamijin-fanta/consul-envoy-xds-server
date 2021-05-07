module github.com/kamijin-fanta/consul-envoy-xds-server

go 1.15

require (
	github.com/cenkalti/backoff/v4 v4.1.0 // indirect
	github.com/envoyproxy/go-control-plane v0.9.7
	github.com/hashicorp/consul/api v1.8.1
	github.com/hashicorp/go-hclog v0.12.0
	github.com/prometheus/client_golang v1.8.0
	github.com/sirupsen/logrus v1.7.0
	github.com/stretchr/testify v1.7.0 // indirect
	google.golang.org/grpc v1.27.0
)
