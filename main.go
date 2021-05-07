package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	envoy_config_core_v3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	envoy_config_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	envoy_service_endpoint_v3 "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
)

var (
	namespace                = "consul_envoy_xds_server"
	serviceListUpdateCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "service_list_update",
	})
	serviceUpdateCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "service_update",
	})
	xdsStreamOpenCounter = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "xds_stream_open",
	})
	xdsStreamCurrentGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "xds_stream_current",
	})
)

var (
	listen     string
	httpListen string
	logLevel   string
	logFormat  string
)

func main() {
	flag.StringVar(&listen, "listen", "127.0.0.1:15000", "gRPC listen address")
	flag.StringVar(&httpListen, "http-listen", "127.0.0.1:15001", "http metrics listen address")
	flag.StringVar(&logLevel, "log-level", "info", "logging level. choose form panic,fatal,error,warn,info,debug,trace.")
	flag.StringVar(&logFormat, "log-format", "text", "log format. choose from json,text.")
	flag.Parse()

	start()
}
func start() {
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		panic(err)
	}
	logger := logrus.New()
	logger.SetLevel(level)
	logger.Infof("set log level with %s", level)
	switch logFormat {
	case "json", "JSON":
		logger.SetFormatter(&logrus.JSONFormatter{})
	}

	lis, err := net.Listen("tcp", listen)
	if err != nil {
		panic(err)
	}

	serviceUpdate := make(chan []*Service)
	stop := make(chan struct{})
	go func() {
		startWatcher(serviceUpdate, logger.WithField("component", "consul-watcher"))
		stop <- struct{}{}
	}()
	go func() {
		startXdsServer(lis, serviceUpdate, logger.WithField("component", "xds-server"))
		stop <- struct{}{}
	}()
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		logger.WithField("addr", httpListen).Info("start metrics http server")
		err := http.ListenAndServe(httpListen, nil)
		if err != nil {
			logger.WithError(err).Fatal("failed start metrics http server")
		}
		stop <- struct{}{}
	}()
	<-stop
}

type ServiceWatcher struct {
	*Service
	Plan    *watch.Plan
	mux     sync.Mutex
	Entries []*api.ServiceEntry
}
type Service struct {
	ServiceName string
	Endpoints   []*ServiceEndpoint
}

type ServiceEndpoint struct {
	Host string
	Port uint32
}

func startWatcher(serviceUpdate chan []*Service, logger *logrus.Entry) {
	config := api.DefaultConfig()

	client, err := api.NewClient(config)
	if err != nil {
		logger.WithError(err).Fatal("failed create new consul client")
	}

	hcLogger := hclog.New(&hclog.LoggerOptions{
		Name: "watch",
	})

	watchServices := map[string]*ServiceWatcher{}
	watchServicesMux := sync.Mutex{}

	servicesWatchPlan, err := watch.Parse(map[string]interface{}{
		"type": "services",
	})
	servicesWatchPlan.Handler = func(u uint64, i interface{}) {
		logger.Debug("change services list")
		serviceListUpdateCounter.Inc()
		watchServicesMux.Lock()
		defer watchServicesMux.Unlock()

		services := i.(map[string][]string)
		for serviceName := range services {
			if _, found := watchServices[serviceName]; found {
				continue
			}
			// new service
			logger.WithField("service", serviceName).Debug("start service watch")

			plan, err := watch.Parse(map[string]interface{}{
				"type":    "service",
				"service": serviceName,
			})
			if err != nil {
				logger.WithError(err).Fatal("failed parse watch plan")
			}
			sn := serviceName
			service := &ServiceWatcher{
				Service: &Service{
					ServiceName: sn,
					Endpoints:   []*ServiceEndpoint{},
				},
				Plan: plan,
			}

			emmitChange := func() {
				serviceUpdateCounter.Inc()
				watchServicesMux.Lock()
				defer watchServicesMux.Unlock()

				updateServices := make([]*Service, 0, len(watchServices))
				for _, service := range watchServices {
					endpoints := make([]*ServiceEndpoint, 0, len(service.Entries))
					for _, se := range service.Entries {
						switch se.Checks.AggregatedStatus() {
						case api.HealthCritical, api.HealthMaint:
							logger.
								WithField("service", sn).
								WithField("status", se.Checks.AggregatedStatus()).
								WithField("addr", se.Node.Address).
								Debug("skip entry")
							continue
						}

						addr := se.Service.Address
						if addr == "" {
							addr = se.Node.Address
						}

						logger.
							WithField("service", sn).
							WithField("status", se.Checks.AggregatedStatus()).
							WithField("node_addr", se.Node.Address).
							WithField("service_addr", fmt.Sprintf("%s:%d", addr, se.Service.Port)).
							Debug("refresh entry")

						endpoints = append(endpoints, &ServiceEndpoint{
							Host: addr,
							Port: uint32(se.Service.Port),
						})
					}
					service.Endpoints = endpoints
					updateServices = append(updateServices, service.Service)
				}
				sort.Slice(updateServices, func(i, j int) bool {
					return updateServices[i].ServiceName > updateServices[i].ServiceName
				})
				serviceUpdate <- updateServices
			}

			checkPlan, err := watch.Parse(map[string]interface{}{
				"type":    "checks",
				"service": serviceName,
			})
			if err != nil {
				logger.WithError(err).Fatal("failed parse checks watch plan")
			}

			checkPlan.Handler = func(u uint64, i interface{}) {
				service.mux.Lock()
				defer service.mux.Unlock()
				receiveChecks := i.([]*api.HealthCheck)

				for _, entry := range service.Entries {
					for _, receiveCheck := range receiveChecks {
						if entry.Service.ID != receiveCheck.ServiceID {
							continue
						}

						for i, check := range entry.Checks {
							if check.CheckID != receiveCheck.CheckID {
								continue
							}
							entry.Checks[i] = receiveCheck
							logger.WithField("check", receiveCheck).Info("check receive")
						}
					}
				}

				logger.
					WithField("service", sn).
					Debug("change service checks")

				emmitChange()
			}

			go func() {
				if err := checkPlan.RunWithClientAndHclog(client, hcLogger); err != nil {
					logger.WithField("service", sn).WithError(err).Fatal("failed running watch checks plan")
				}
			}()

			time.Sleep(1 * time.Second) // start service watch after first checks fetch

			plan.Handler = func(u uint64, i interface{}) {
				service.mux.Lock()
				defer service.mux.Unlock()
				entries := i.([]*api.ServiceEntry)
				service.Entries = entries

				logger.
					WithField("service", sn).
					Debug("change service detail")
				emmitChange()
			}
			watchServices[serviceName] = service
			go func() {
				if err := plan.RunWithClientAndHclog(client, hcLogger); err != nil {
					logger.WithField("service", sn).WithError(err).Fatal("failed running watch watch service plan")
				}
			}()

			// todo stop to plan
		}

		for serviceName, plan := range watchServices {
			if _, found := services[serviceName]; found {
				continue
			}
			delete(watchServices, serviceName)
			logger.WithField("service", serviceName).Debug("stop service watch")
			plan.Plan.Stop()
		}
	}

	if err != nil {
		logger.WithError(err).Fatal("failed watch services")
	}
	if err := servicesWatchPlan.RunWithClientAndHclog(client, hcLogger); err != nil {
		logger.WithError(err).Fatal("failed running watch watch services plan")
	}
}

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
