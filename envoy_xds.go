package main

import (
	"context"
	"fmt"
	"net"
	"time"

	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_config_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoy_service_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

var _ cache.NodeHash = &StandardNodeHash{}

type StandardNodeHash struct{}

func (s *StandardNodeHash) ID(node *envoy_config_core_v3.Node) string {
	return "default"
}

func startXdsServer(listener net.Listener, serviceUpdate <-chan []*Service, logger *logrus.Entry) {
	ctx := context.Background()

	snapshotCache := cache.NewSnapshotCache(false, &StandardNodeHash{}, nil)
	callbacks := server.CallbackFuncs{
		StreamOpenFunc: func(ctx context.Context, i int64, s string) error {
			xdsStreamOpenCounter.Inc()
			xdsStreamCurrentGauge.Inc()
			return nil
		},
		StreamClosedFunc: func(i int64) {
			xdsStreamCurrentGauge.Dec()
		},
		StreamRequestFunc:  nil,
		StreamResponseFunc: nil,
		FetchRequestFunc:   nil,
		FetchResponseFunc:  nil,
	}
	srv := server.NewServer(ctx, snapshotCache, callbacks)

	go func() {
		for {
			upstreams := <-serviceUpdate
			err := snapshotCache.SetSnapshot("default", generateSnapshot(upstreams))
			if err != nil {
				logger.WithError(err).Fatal("set snapshot error")
			}
		}
	}()

	grpcServer := grpc.NewServer()
	envoy_service_endpoint_v3.RegisterEndpointDiscoveryServiceServer(grpcServer, srv)

	err := grpcServer.Serve(listener)
	if err != nil {
		logger.WithError(err).Fatal("error on start grpc server")
	}
}

func generateSnapshot(services []*Service) cache.Snapshot {
	var resources []types.Resource

	for _, service := range services {
		endpoints := make([]*envoy_config_endpoint_v3.LbEndpoint, 0, len(services))
		for _, endpoint := range service.Endpoints {
			endpoint := &envoy_config_endpoint_v3.LbEndpoint{
				HostIdentifier: &envoy_config_endpoint_v3.LbEndpoint_Endpoint{
					Endpoint: &envoy_config_endpoint_v3.Endpoint{
						Address: &envoy_config_core_v3.Address{
							Address: &envoy_config_core_v3.Address_SocketAddress{
								SocketAddress: &envoy_config_core_v3.SocketAddress{
									Protocol: envoy_config_core_v3.SocketAddress_TCP,
									Address:  endpoint.Host,
									PortSpecifier: &envoy_config_core_v3.SocketAddress_PortValue{
										PortValue: endpoint.Port,
									},
								},
							},
						},
					},
				},
			}
			endpoints = append(endpoints, endpoint)
		}

		assignment := &envoy_config_endpoint_v3.ClusterLoadAssignment{
			ClusterName: service.ServiceName,
			Endpoints: []*envoy_config_endpoint_v3.LocalityLbEndpoints{
				{
					LbEndpoints: endpoints,
				},
			},
		}
		resources = append(resources, assignment)
	}
	return cache.NewSnapshot(fmt.Sprintf("%d", time.Now().UnixNano()), resources, nil, nil, nil, nil, nil)
}
