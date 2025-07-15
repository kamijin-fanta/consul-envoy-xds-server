package main

import (
	"context"
	"sort"
	"sync"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/sirupsen/logrus"
)

type NomadServiceWatcher struct {
	sync.Mutex
	logger        *logrus.Entry
	serviceUpdate chan []*Service
	client        *nomadapi.Client
	services      map[string]*Service
	stop          chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewNomadServiceWatcher(logger *logrus.Entry, client *nomadapi.Client) *NomadServiceWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &NomadServiceWatcher{
		logger:        logger,
		serviceUpdate: make(chan []*Service, 10),
		client:        client,
		services:      make(map[string]*Service),
		stop:          make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
}

func (n *NomadServiceWatcher) Start() error {
	go n.watchServices()
	return nil
}

func (n *NomadServiceWatcher) Stop() {
	n.cancel()
	close(n.stop)
	close(n.serviceUpdate)
}

func (n *NomadServiceWatcher) ServiceUpdate() <-chan []*Service {
	return n.serviceUpdate
}

func (n *NomadServiceWatcher) watchServices() {
	var lastIndex uint64 = 0
	
	for {
		select {
		case <-n.ctx.Done():
			return
		default:
			services, meta, err := n.client.Services().List(&nomadapi.QueryOptions{
				WaitIndex: lastIndex,
				WaitTime:  5 * time.Minute,
			})
			
			if err != nil {
				n.logger.WithError(err).Error("failed to list Nomad services")
				time.Sleep(10 * time.Second)
				continue
			}
			
			if meta.LastIndex > lastIndex {
				lastIndex = meta.LastIndex
				n.processServiceList(services)
			}
		}
	}
}

func (n *NomadServiceWatcher) processServiceList(serviceStubs []*nomadapi.ServiceRegistrationListStub) {
	n.Lock()
	defer n.Unlock()
	
	currentServices := make(map[string]bool)
	
	for _, stub := range serviceStubs {
		for _, serviceStub := range stub.Services {
			currentServices[serviceStub.ServiceName] = true
			
			if _, exists := n.services[serviceStub.ServiceName]; !exists {
				n.services[serviceStub.ServiceName] = &Service{
					ServiceName: serviceStub.ServiceName,
					Endpoints:   []*ServiceEndpoint{},
				}
				go n.watchServiceDetail(serviceStub.ServiceName)
			}
		}
	}
	
	// Remove services that no longer exist
	for serviceName := range n.services {
		if !currentServices[serviceName] {
			delete(n.services, serviceName)
		}
	}
	
	n.emitServices()
}

func (n *NomadServiceWatcher) watchServiceDetail(serviceName string) {
	var lastIndex uint64 = 0
	
	for {
		select {
		case <-n.ctx.Done():
			return
		default:
			services, meta, err := n.client.Services().Get(serviceName, &nomadapi.QueryOptions{
				WaitIndex: lastIndex,
				WaitTime:  5 * time.Minute,
			})
			
			if err != nil {
				n.logger.WithField("service", serviceName).WithError(err).Error("failed to get Nomad service details")
				time.Sleep(10 * time.Second)
				continue
			}
			
			if meta.LastIndex > lastIndex {
				lastIndex = meta.LastIndex
				n.updateServiceEndpoints(serviceName, services)
			}
		}
	}
}

func (n *NomadServiceWatcher) updateServiceEndpoints(serviceName string, registrations []*nomadapi.ServiceRegistration) {
	n.Lock()
	defer n.Unlock()
	
	service, exists := n.services[serviceName]
	if !exists {
		return
	}
	
	endpoints := make([]*ServiceEndpoint, 0, len(registrations))
	
	for _, reg := range registrations {
		endpoints = append(endpoints, &ServiceEndpoint{
			Host: reg.Address,
			Port: uint32(reg.Port),
		})
		
		n.logger.
			WithField("service", serviceName).
			WithField("address", reg.Address).
			WithField("port", reg.Port).
			Debug("nomad: refresh service endpoint")
	}
	
	service.Endpoints = endpoints
	serviceUpdateCounter.Inc()
	n.emitServices()
}

func (n *NomadServiceWatcher) emitServices() {
	services := make([]*Service, 0, len(n.services))
	for _, service := range n.services {
		// Create a copy to avoid race conditions
		serviceCopy := &Service{
			ServiceName: service.ServiceName,
			Endpoints:   make([]*ServiceEndpoint, len(service.Endpoints)),
		}
		copy(serviceCopy.Endpoints, service.Endpoints)
		services = append(services, serviceCopy)
	}
	
	sort.Slice(services, func(i, j int) bool {
		return services[i].ServiceName < services[j].ServiceName
	})
	
	select {
	case n.serviceUpdate <- services:
	default:
		// Channel is full, skip this update
		n.logger.Warn("nomad: service update channel is full, skipping update")
	}
}