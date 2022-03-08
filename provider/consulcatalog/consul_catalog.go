package consulcatalog

import (
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/BurntSushi/ty/fun"
	"github.com/cenk/backoff"
	"github.com/hashicorp/consul/api"
	"github.com/traefik/traefik/job"
	"github.com/traefik/traefik/log"
	"github.com/traefik/traefik/provider"
	"github.com/traefik/traefik/provider/label"
	"github.com/traefik/traefik/safe"
	"github.com/traefik/traefik/types"
)

const (
	// DefaultWatchWaitTime is the duration to wait when polling consul
	DefaultWatchWaitTime = 15 * time.Second
)

var _ provider.Provider = (*Provider)(nil)

// Provider holds configurations of the Consul catalog provider.
type Provider struct {
	provider.BaseProvider `mapstructure:",squash" export:"true"`
	Endpoint              string `description:"Consul server endpoint"`
	Domain                string `description:"Default domain used"`
	Stale                 bool   `description:"Use stale consistency for catalog reads" export:"true"`
	// Note: Cache is *only used for health*.  This is due to the Health APIs using background streaming and
	//  not the normal cache refresh state.  This means super stale data is a lot harder to get and we
	//  don't have to worry about race conditions.
	Cache                bool             `description:"Use cache for health reads" export:"true"`
	ExposedByDefault     bool             `description:"Expose Consul services by default" export:"true"`
	Prefix               string           `description:"Prefix used for Consul catalog tags" export:"true"`
	StrictChecks         bool             `description:"Keep a Consul node only if all checks status are passing" export:"true"`
	AllStatuses          bool             `description:"Allow all status checks, even if failing" export:"true"`
	FrontEndRule         string           `description:"Frontend rule used for Consul services" export:"true"`
	Filter               string           `description:"Filter to pass to Consul servers" export:"true"`
	FilterCatalogByTag   string           `description:"Tag to filter the Consul catalog by" export:"true"`
	RefreshInterval      int              `description:"Minimum interval between Catalog checks in seconds" export:"true"`
	TLS                  *types.ClientTLS `description:"Enable TLS support" export:"true"`
	client               *api.Client
	frontEndRuleTemplate *template.Template
}

// Service represent a Consul service.
type Service struct {
	Name      string
	Tags      []string
	Nodes     []string
	Addresses []string
	Ports     []int
}

type serviceUpdate struct {
	ServiceName       string
	ParentServiceName string
	Attributes        []string
	TraefikLabels     map[string]string
}

type frontendSegment struct {
	Name   string
	Labels map[string]string
}

type catalogUpdate struct {
	Service *serviceUpdate
	Nodes   []*api.ServiceEntry
}

type nodeSorter []*api.ServiceEntry

func (a nodeSorter) Len() int {
	return len(a)
}

func (a nodeSorter) Swap(i int, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a nodeSorter) Less(i int, j int) bool {
	lEntry := a[i]
	rEntry := a[j]

	ls := strings.ToLower(lEntry.Service.Service)
	lr := strings.ToLower(rEntry.Service.Service)

	if ls != lr {
		return ls < lr
	}
	if lEntry.Service.Address != rEntry.Service.Address {
		return lEntry.Service.Address < rEntry.Service.Address
	}
	if lEntry.Node.Address != rEntry.Node.Address {
		return lEntry.Node.Address < rEntry.Node.Address
	}
	return lEntry.Service.Port < rEntry.Service.Port
}

// Init the provider
func (p *Provider) Init(constraints types.Constraints) error {
	err := p.BaseProvider.Init(constraints)
	if err != nil {
		return err
	}

	client, err := p.createClient()
	if err != nil {
		return err
	}

	p.client = client
	p.setupFrontEndRuleTemplate()

	return nil
}

// Provide allows the consul catalog provider to provide configurations to traefik
// using the given configuration channel.
func (p *Provider) Provide(configurationChan chan<- types.ConfigMessage, pool *safe.Pool) error {
	pool.Go(func(stop chan bool) {
		notify := func(err error, time time.Duration) {
			log.Errorf("Consul connection error %+v, retrying in %s", err, time)
		}
		operation := func() error {
			return p.watch(configurationChan, stop)
		}
		errRetry := backoff.RetryNotify(safe.OperationWithRecover(operation), job.NewBackOff(backoff.NewExponentialBackOff()), notify)
		if errRetry != nil {
			log.Errorf("Cannot connect to consul server %+v", errRetry)
		}
	})
	return nil
}

func (p *Provider) createClient() (*api.Client, error) {
	config := api.DefaultConfig()
	config.Address = p.Endpoint
	if p.TLS != nil {
		tlsConfig, err := p.TLS.CreateTLSConfig()
		if err != nil {
			return nil, err
		}

		config.Scheme = "https"
		config.Transport.TLSClientConfig = tlsConfig
	}

	client, err := api.NewClient(config)
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (p *Provider) watch(configurationChan chan<- types.ConfigMessage, stop chan bool) error {
	stopCh := make(chan struct{})
	watchCh := make(chan map[string][]string)
	errorCh := make(chan error)
	ticker := time.NewTicker(time.Duration(p.RefreshInterval) * time.Second)

	var errorOnce sync.Once
	notifyError := func(err error) {
		errorOnce.Do(func() {
			errorCh <- err
		})
	}

	defer close(stopCh)
	defer close(watchCh)

	safe.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				data, err := p.getCatalogServices(notifyError)
				if err != nil {
					notifyError(err)
				}
				log.Debug("List of services being updated")
				nodes, err := p.getNodes(data)
				if err != nil {
					notifyError(err)
				}
				configuration := p.buildConfiguration(nodes)
				configurationChan <- types.ConfigMessage{
					ProviderName:  "consul_catalog",
					Configuration: configuration,
				}
				log.Debug("List of services updated")
			}
		}
	})

	for {
		select {
		case <-stop:
			return nil
		case err := <-errorCh:
			return err
		}
	}
}

func (p *Provider) getCatalogServices(notifyError func(error)) (map[string][]string, error) {
	catalog := p.client.Catalog()
	options := &api.QueryOptions{AllowStale: p.Stale}
	data, _, err := catalog.Services(options)
	if err != nil {
		log.Errorf("Failed to list services: %v", err)
		notifyError(err)
		return nil, err
	}

	if len(p.FilterCatalogByTag) != 0 {
		filterServicesByTag(data, p.FilterCatalogByTag)
	}
	return data, nil
}

func (p *Provider) getNodes(index map[string][]string) ([]catalogUpdate, error) {
	visited := make(map[string]bool)

	var nodes []catalogUpdate
	for service := range index {
		name := strings.ToLower(service)
		if !strings.Contains(name, " ") && !visited[name] {
			visited[name] = true
			log.WithField("service", name).Debug("Fetching service")
			healthy, err := p.healthyNodes(name)
			if err != nil {
				return nil, err
			}
			// healthy.Nodes can be empty if constraints do not match, without throwing error
			if healthy.Service != nil && len(healthy.Nodes) > 0 {
				nodes = append(nodes, healthy)
			}
		}
	}
	return nodes, nil
}

func hasChanged(current map[string]Service, previous map[string]Service) bool {
	if len(current) != len(previous) {
		return true
	}
	addedServiceKeys, removedServiceKeys := getChangedServiceKeys(current, previous)
	return len(removedServiceKeys) > 0 || len(addedServiceKeys) > 0 || hasServiceChanged(current, previous)
}

func getChangedServiceKeys(current map[string]Service, previous map[string]Service) ([]string, []string) {
	currKeySet := fun.Set(fun.Keys(current).([]string)).(map[string]bool)
	prevKeySet := fun.Set(fun.Keys(previous).([]string)).(map[string]bool)

	addedKeys := fun.Difference(currKeySet, prevKeySet).(map[string]bool)
	removedKeys := fun.Difference(prevKeySet, currKeySet).(map[string]bool)

	return fun.Keys(addedKeys).([]string), fun.Keys(removedKeys).([]string)
}

func hasServiceChanged(current map[string]Service, previous map[string]Service) bool {
	for key, value := range current {
		if prevValue, ok := previous[key]; ok {
			addedNodesKeys, removedNodesKeys := getChangedStringKeys(value.Nodes, prevValue.Nodes)
			if len(addedNodesKeys) > 0 || len(removedNodesKeys) > 0 {
				return true
			}
			addedTagsKeys, removedTagsKeys := getChangedStringKeys(value.Tags, prevValue.Tags)
			if len(addedTagsKeys) > 0 || len(removedTagsKeys) > 0 {
				return true
			}
			addedAddressesKeys, removedAddressesKeys := getChangedStringKeys(value.Addresses, prevValue.Addresses)
			if len(addedAddressesKeys) > 0 || len(removedAddressesKeys) > 0 {
				return true
			}
			addedPortsKeys, removedPortsKeys := getChangedIntKeys(value.Ports, prevValue.Ports)
			if len(addedPortsKeys) > 0 || len(removedPortsKeys) > 0 {
				return true
			}
		}
	}
	return false
}

func getChangedStringKeys(currState []string, prevState []string) ([]string, []string) {
	currKeySet := fun.Set(currState).(map[string]bool)
	prevKeySet := fun.Set(prevState).(map[string]bool)

	addedKeys := fun.Difference(currKeySet, prevKeySet).(map[string]bool)
	removedKeys := fun.Difference(prevKeySet, currKeySet).(map[string]bool)

	return fun.Keys(addedKeys).([]string), fun.Keys(removedKeys).([]string)
}

func getChangedHealth(current map[string][]string, previous map[string][]string) ([]string, []string, []string) {
	currKeySet := fun.Set(fun.Keys(current).([]string)).(map[string]bool)
	prevKeySet := fun.Set(fun.Keys(previous).([]string)).(map[string]bool)

	addedKeys := fun.Difference(currKeySet, prevKeySet).(map[string]bool)
	removedKeys := fun.Difference(prevKeySet, currKeySet).(map[string]bool)

	var changedKeys []string

	for key, value := range current {
		if prevValue, ok := previous[key]; ok {
			addedNodesKeys, removedNodesKeys := getChangedStringKeys(value, prevValue)
			if len(addedNodesKeys) > 0 || len(removedNodesKeys) > 0 {
				changedKeys = append(changedKeys, key)
			}
		}
	}

	return fun.Keys(addedKeys).([]string), fun.Keys(removedKeys).([]string), changedKeys
}

func getChangedIntKeys(currState []int, prevState []int) ([]int, []int) {
	currKeySet := fun.Set(currState).(map[int]bool)
	prevKeySet := fun.Set(prevState).(map[int]bool)

	addedKeys := fun.Difference(currKeySet, prevKeySet).(map[int]bool)
	removedKeys := fun.Difference(prevKeySet, currKeySet).(map[int]bool)

	return fun.Keys(addedKeys).([]int), fun.Keys(removedKeys).([]int)
}

func getServiceIds(services []*api.CatalogService) []string {
	var serviceIds []string
	for _, service := range services {
		serviceIds = append(serviceIds, service.ID)
	}
	return serviceIds
}

func getServicePorts(services []*api.CatalogService) []int {
	var servicePorts []int
	for _, service := range services {
		servicePorts = append(servicePorts, service.ServicePort)
	}
	return servicePorts
}

func getServiceAddresses(services []*api.CatalogService) []string {
	var serviceAddresses []string
	for _, service := range services {
		serviceAddresses = append(serviceAddresses, service.ServiceAddress)
	}
	return serviceAddresses
}

func (p *Provider) healthyNodes(service string) (catalogUpdate, error) {
	health := p.client.Health()
	// You can't filter with assigning passingOnly here, nodeFilter will do this later
	data, _, err := health.Service(service, "", false, &api.QueryOptions{AllowStale: p.Stale, UseCache: p.Cache})
	if err != nil {
		log.WithError(err).Errorf("Failed to fetch details of %s", service)
		return catalogUpdate{}, err
	}

	nodes := fun.Filter(func(node *api.ServiceEntry) bool {
		return p.nodeFilter(service, node)
	}, data).([]*api.ServiceEntry)

	// Merge tags of nodes matching constraints, in a single slice.
	tags := fun.Foldl(func(node *api.ServiceEntry, set []string) []string {
		return fun.Keys(fun.Union(
			fun.Set(set),
			fun.Set(node.Service.Tags),
		).(map[string]bool)).([]string)
	}, []string{}, nodes).([]string)

	labels := tagsToNeutralLabels(tags, p.Prefix)

	return catalogUpdate{
		Service: &serviceUpdate{
			ServiceName:   service,
			Attributes:    tags,
			TraefikLabels: labels,
		},
		Nodes: nodes,
	}, nil
}

func (p *Provider) nodeFilter(service string, node *api.ServiceEntry) bool {
	// Filter disabled application.
	if !p.isServiceEnabled(node) {
		log.Debugf("Filtering disabled Consul service %s", service)
		return false
	}

	// Filter by constraints.
	constraintTags := p.getConstraintTags(node.Service.Tags)
	ok, failingConstraint := p.MatchConstraints(constraintTags)
	if !ok && failingConstraint != nil {
		log.Debugf("Service %v pruned by '%v' constraint", service, failingConstraint.String())
		return false
	}

	return p.hasPassingChecks(node)
}

func (p *Provider) isServiceEnabled(node *api.ServiceEntry) bool {
	rawValue := getTag(p.getPrefixedName(label.SuffixEnable), node.Service.Tags, "")

	if len(rawValue) == 0 {
		return p.ExposedByDefault
	}

	value, err := strconv.ParseBool(rawValue)
	if err != nil {
		log.Errorf("Invalid value for %s: %s", label.SuffixEnable, rawValue)
		return p.ExposedByDefault
	}
	return value
}

func (p *Provider) getConstraintTags(tags []string) []string {
	var values []string

	prefix := p.getPrefixedName("tags=")
	for _, tag := range tags {
		// We look for a Consul tag named 'traefik.tags' (unless different 'prefix' is configured)
		if strings.HasPrefix(strings.ToLower(tag), prefix) {
			// If 'traefik.tags=' tag is found, take the tag value and split by ',' adding the result to the list to be returned
			splitedTags := label.SplitAndTrimString(tag[len(prefix):], ",")
			values = append(values, splitedTags...)
		}
	}

	return values
}

func (p *Provider) hasPassingChecks(node *api.ServiceEntry) bool {
	status := node.Checks.AggregatedStatus()
	// We need to look for one specific situation.
	// If serfHealth is failing, the entire node is probably offline and we should always filter it.
	for _, c := range node.Checks {
		if c.CheckID == "serfHealth" && c.Status != "passing" {
			return false
		}
	}
	// When AllStatuses is true, beyond the above comment, always accept a node.
	return status == "passing" || !p.StrictChecks && status == "warning" || p.AllStatuses
}

func (p *Provider) generateFrontends(service *serviceUpdate) []*serviceUpdate {
	frontends := make([]*serviceUpdate, 0)
	// to support <prefix>.frontend.xxx
	frontends = append(frontends, &serviceUpdate{
		ServiceName:       service.ServiceName,
		ParentServiceName: service.ServiceName,
		Attributes:        service.Attributes,
		TraefikLabels:     service.TraefikLabels,
	})

	// loop over children of <prefix>.frontends.*
	for _, frontend := range getSegments(label.Prefix+"frontends", label.Prefix, service.TraefikLabels) {
		frontends = append(frontends, &serviceUpdate{
			ServiceName:       service.ServiceName + "-" + frontend.Name,
			ParentServiceName: service.ServiceName,
			Attributes:        service.Attributes,
			TraefikLabels:     frontend.Labels,
		})
	}

	return frontends
}

func getSegments(path string, prefix string, tree map[string]string) []*frontendSegment {
	segments := make([]*frontendSegment, 0)
	// find segment names
	segmentNames := make(map[string]bool)
	for key := range tree {
		if strings.HasPrefix(key, path+".") {
			segmentNames[strings.SplitN(strings.TrimPrefix(key, path+"."), ".", 2)[0]] = true
		}
	}

	// get labels for each segment found
	for segment := range segmentNames {
		labels := make(map[string]string)
		for key, value := range tree {
			if strings.HasPrefix(key, path+"."+segment) {
				labels[prefix+"frontend"+strings.TrimPrefix(key, path+"."+segment)] = value
			}
		}
		segments = append(segments, &frontendSegment{
			Name:   segment,
			Labels: labels,
		})
	}

	return segments
}

func filterServicesByTag(services map[string][]string, filterTag string) {
	for service, tags := range services {
		filterTagExists := false
		for _, tag := range tags {
			if strings.Compare(tag, filterTag) == 0 {
				filterTagExists = true
				break
			}
		}
		if !filterTagExists {
			delete(services, service)
		}
	}
}
