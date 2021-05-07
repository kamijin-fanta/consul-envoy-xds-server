package main

import (
	"flag"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"net"
	"net/http"
	"sync"
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
		//startWatcher(serviceUpdate, logger.WithField("component", "consul-watcher"))

		config := api.DefaultConfig()
		client, err := api.NewClient(config)
		if err != nil {
			logger.WithError(err).Fatal("failed create new consul client")
		}
		hcLogger := hclog.New(&hclog.LoggerOptions{
			Name: "watch",
		})
		registry := &ServiceRegistry{
			logger:        logger.WithField("component", "consul-watcher"),
			serviceUpdate: serviceUpdate,
			services:      map[string]*WatchedService{},
		}
		registry.Start(client, hcLogger)
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
