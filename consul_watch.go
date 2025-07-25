package main

import (
	"fmt"
	"sort"
	"sync"

	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/api/watch"
	"github.com/hashicorp/go-hclog"
	"github.com/sirupsen/logrus"
)

type ConsulServiceWatcher struct {
	sync.Mutex
	logger            *logrus.Entry
	serviceUpdate     chan []*Service
	services          map[string]*WatchedService
	client            *api.Client
	hcLogger          hclog.Logger
	stop              chan struct{}
	servicesWatchPlan *watch.Plan
}

func NewConsulServiceWatcher(logger *logrus.Entry, client *api.Client, hcLogger hclog.Logger) *ConsulServiceWatcher {
	return &ConsulServiceWatcher{
		logger:        logger,
		serviceUpdate: make(chan []*Service, 10),
		services:      map[string]*WatchedService{},
		client:        client,
		hcLogger:      hcLogger,
		stop:          make(chan struct{}),
	}
}

func (r *ConsulServiceWatcher) Emit() {
	r.Lock()
	defer r.Unlock()

	updateServices := make([]*Service, 0, len(r.services))
	for _, service := range r.services {
		func() {
			service.Lock()
			defer service.Unlock()

			endpoints := make([]*ServiceEndpoint, 0, len(service.Entries))
			for _, se := range service.Entries {
				switch se.Checks.AggregatedStatus() {
				case api.HealthCritical, api.HealthMaint:
					r.logger.
						WithField("service", service.ServiceName).
						WithField("status", se.Checks.AggregatedStatus()).
						WithField("addr", se.Node.Address).
						Debug("skip entry")
					continue
				}

				addr := se.Service.Address
				if addr == "" {
					addr = se.Node.Address
				}

				r.logger.
					WithField("service", service.ServiceName).
					WithField("status", se.Checks.AggregatedStatus()).
					WithField("node_addr", se.Node.Address).
					WithField("service_addr", fmt.Sprintf("%s:%d", addr, se.Service.Port)).
					Debug("refresh entry")

				endpoints = append(endpoints, &ServiceEndpoint{
					Host:   addr,
					Port:   uint32(se.Service.Port),
					Source: "consul",
				})
			}
			service.Endpoints = endpoints
			updateServices = append(updateServices, &service.Service)
		}()
	}
	sort.Slice(updateServices, func(i, j int) bool {
		return updateServices[i].ServiceName < updateServices[j].ServiceName
	})

	r.serviceUpdate <- updateServices
}

func (r *ConsulServiceWatcher) Start() error {
	servicesWatchPlan, err := watch.Parse(map[string]interface{}{
		"type": "services",
	})
	if err != nil {
		r.logger.WithError(err).Fatal("failed parse services watch plan")
		return err
	}
	r.servicesWatchPlan = servicesWatchPlan

	servicesWatchPlan.Handler = func(u uint64, i interface{}) {
		services := i.(map[string][]string)
		r.Lock()
		defer r.Unlock()

		// start watch
		for serviceName := range services {
			if _, found := r.services[serviceName]; found {
				continue
			}
			watchedService := &WatchedService{
				Service: Service{
					ServiceName: serviceName,
					Endpoints:   nil,
				},
				logger: r.logger,
				handleChange: func() {
					go r.Emit()
				},
			}
			r.services[serviceName] = watchedService
			watchedService.Start(r.client, r.hcLogger)
		}

		// stop watch
		for serviceName, service := range r.services {
			if _, found := services[serviceName]; found {
				continue
			}
			delete(r.services, serviceName)
			r.logger.WithField("service", serviceName).Debug("stop service watch")
			service.checksPlan.Stop()
			service.servicePlan.Stop()
		}
	}

	go func() {
		if err := servicesWatchPlan.RunWithClientAndHclog(r.client, r.hcLogger); err != nil {
			r.logger.WithError(err).Error("failed running watch services plan")
		}
	}()
	return nil
}

func (r *ConsulServiceWatcher) Stop() {
	r.Lock()
	defer r.Unlock()

	if r.servicesWatchPlan != nil {
		r.servicesWatchPlan.Stop()
	}

	for _, service := range r.services {
		if service.checksPlan != nil {
			service.checksPlan.Stop()
		}
		if service.servicePlan != nil {
			service.servicePlan.Stop()
		}
	}

	close(r.stop)
	close(r.serviceUpdate)
}

func (r *ConsulServiceWatcher) ServiceUpdate() <-chan []*Service {
	return r.serviceUpdate
}

type WatchedService struct {
	Service
	sync.Mutex

	handleChange func()
	logger       *logrus.Entry
	checksPlan   *watch.Plan
	servicePlan  *watch.Plan
	Entries      []*api.ServiceEntry
}

func (w *WatchedService) Start(client *api.Client, hcLogger hclog.Logger) error {
	logger := w.logger.
		WithField("service", w.ServiceName)

	servicePlan, err := watch.Parse(map[string]interface{}{
		"type":    "service",
		"service": w.ServiceName,
	})
	if err != nil {
		logger.WithError(err).Fatal("watch: failed parse service watch plan")
		return err
	}
	w.servicePlan = servicePlan
	servicePlan.Handler = func(u uint64, i interface{}) {
		w.Lock()
		defer w.Unlock()
		entries := i.([]*api.ServiceEntry)
		w.Entries = entries

		serviceUpdateCounter.Inc()
		w.handleChange()
	}
	runServicePlan := func() {
		logger.Debug("watch: start service plan")
		if err := servicePlan.RunWithClientAndHclog(client, hcLogger); err != nil {
			w.logger.WithField("service", w.ServiceName).WithError(err).Fatal("failed running watch watch service plan")
		}
	}

	checksPlan, err := watch.Parse(map[string]interface{}{
		"type":    "checks",
		"service": w.ServiceName,
	})
	if err != nil {
		logger.WithError(err).Fatal("failed parse checks watch plan")
		return err
	}
	w.checksPlan = checksPlan
	isFirst := true
	checksPlan.Handler = func(u uint64, arg interface{}) {
		w.Lock()
		defer w.Unlock()

		if isFirst {
			go runServicePlan()
			isFirst = false
		}

		receiveChecks := arg.([]*api.HealthCheck)

		updates := 0
		for _, entry := range w.Entries {
			for _, receiveCheck := range receiveChecks {
				if entry.Service.ID != receiveCheck.ServiceID {
					continue
				}

				for i, check := range entry.Checks {
					if check.CheckID != receiveCheck.CheckID {
						continue
					}
					entry.Checks[i] = receiveCheck
					updates++
				}
			}
		}

		logger.
			WithField("updates", updates).
			WithField("Entries", len(w.Entries)).
			WithField("receiveChecks", len(receiveChecks)).
			Debug("watch: change service checks")

		serviceUpdateCounter.Inc()
		w.handleChange()
	}

	runChecksPlan := func() {
		logger.Debug("watch: start checks watch plan")
		if err := checksPlan.RunWithClientAndHclog(client, hcLogger); err != nil {
			w.logger.WithField("service", w.ServiceName).WithError(err).Fatal("failed running watch checks plan")
		}
	}

	go runChecksPlan()
	return nil
}
