// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/version"
	apiv1 "k8s.io/api/core/v1"
	disv1 "k8s.io/api/discovery/v1"
	networkv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp" // Required to get the GCP auth provider working.
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/util/strutil"
)

const (
	// metaLabelPrefix is the meta prefix used for all meta labels.
	// in this discovery.
	metaLabelPrefix = model.MetaLabelPrefix + "kubernetes_"
	namespaceLabel  = metaLabelPrefix + "namespace"
	presentValue    = model.LabelValue("true")
)

// DefaultSDConfig is the default Kubernetes SD configuration.
var DefaultSDConfig = SDConfig{
	HTTPClientConfig: config.DefaultHTTPClientConfig,
}

func init() {
	discovery.RegisterConfig(&SDConfig{})
}

// Role is role of the service in Kubernetes.
type Role string

// The valid options for Role.
const (
	RoleNode          Role = "node"
	RolePod           Role = "pod"
	RoleService       Role = "service"
	RoleEndpoint      Role = "endpoints"
	RoleEndpointSlice Role = "endpointslice"
	RoleIngress       Role = "ingress"
)

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *Role) UnmarshalYAML(unmarshal func(interface{}) error) error {
	if err := unmarshal((*string)(c)); err != nil {
		return err
	}
	switch *c {
	case RoleNode, RolePod, RoleService, RoleEndpoint, RoleEndpointSlice, RoleIngress:
		return nil
	default:
		return fmt.Errorf("unknown Kubernetes SD role %q", *c)
	}
}

func (c Role) String() string {
	return string(c)
}

const (
	MetricLabelRoleAdd    = "add"
	MetricLabelRoleDelete = "delete"
	MetricLabelRoleUpdate = "update"
)

// SDConfig is the configuration for Kubernetes service discovery.
type SDConfig struct {
	APIServer          config.URL              `yaml:"api_server,omitempty"`
	Role               Role                    `yaml:"role"`
	KubeConfig         string                  `yaml:"kubeconfig_file"`
	HTTPClientConfig   config.HTTPClientConfig `yaml:",inline"`
	NamespaceDiscovery NamespaceDiscovery      `yaml:"namespaces,omitempty"`
	Selectors          []SelectorConfig        `yaml:"selectors,omitempty"`
	AttachMetadata     AttachMetadataConfig    `yaml:"attach_metadata,omitempty"`
}

// NewDiscovererMetrics implements discovery.Config.
func (*SDConfig) NewDiscovererMetrics(reg prometheus.Registerer, rmi discovery.RefreshMetricsInstantiator) discovery.DiscovererMetrics {
	return newDiscovererMetrics(reg, rmi)
}

// Name returns the name of the Config.
func (*SDConfig) Name() string { return "kubernetes" }

// NewDiscoverer returns a Discoverer for the Config.
func (c *SDConfig) NewDiscoverer(opts discovery.DiscovererOptions) (discovery.Discoverer, error) {
	return New(opts.Logger, opts.Metrics, c)
}

// SetDirectory joins any relative file paths with dir.
func (c *SDConfig) SetDirectory(dir string) {
	c.HTTPClientConfig.SetDirectory(dir)
	c.KubeConfig = config.JoinDir(dir, c.KubeConfig)
}

type roleSelector struct {
	node          resourceSelector
	pod           resourceSelector
	service       resourceSelector
	endpoints     resourceSelector
	endpointslice resourceSelector
	ingress       resourceSelector
}

type SelectorConfig struct {
	Role  Role   `yaml:"role,omitempty"`
	Label string `yaml:"label,omitempty"`
	Field string `yaml:"field,omitempty"`
}

type resourceSelector struct {
	label string
	field string
}

// AttachMetadataConfig is the configuration for attaching additional metadata
// coming from namespaces or nodes on which the targets are scheduled.
type AttachMetadataConfig struct {
	Node      bool `yaml:"node"`
	Namespace bool `yaml:"namespace"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *SDConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = DefaultSDConfig
	type plain SDConfig
	err := unmarshal((*plain)(c))
	if err != nil {
		return err
	}
	if c.Role == "" {
		return errors.New("role missing (one of: pod, service, endpoints, endpointslice, node, ingress)")
	}
	err = c.HTTPClientConfig.Validate()
	if err != nil {
		return err
	}
	if c.APIServer.URL != nil && c.KubeConfig != "" {
		// Api-server and kubeconfig_file are mutually exclusive
		return errors.New("cannot use 'kubeconfig_file' and 'api_server' simultaneously")
	}
	if c.KubeConfig != "" && !reflect.DeepEqual(c.HTTPClientConfig, config.DefaultHTTPClientConfig) {
		// Kubeconfig_file and custom http config are mutually exclusive
		return errors.New("cannot use a custom HTTP client configuration together with 'kubeconfig_file'")
	}
	if c.APIServer.URL == nil && !reflect.DeepEqual(c.HTTPClientConfig, config.DefaultHTTPClientConfig) {
		return errors.New("to use custom HTTP client configuration please provide the 'api_server' URL explicitly")
	}
	if c.APIServer.URL != nil && c.NamespaceDiscovery.IncludeOwnNamespace {
		return errors.New("cannot use 'api_server' and 'namespaces.own_namespace' simultaneously")
	}
	if c.KubeConfig != "" && c.NamespaceDiscovery.IncludeOwnNamespace {
		return errors.New("cannot use 'kubeconfig_file' and 'namespaces.own_namespace' simultaneously")
	}

	foundSelectorRoles := make(map[Role]struct{})
	allowedSelectors := map[Role][]string{
		RolePod:           {string(RolePod)},
		RoleService:       {string(RoleService)},
		RoleEndpointSlice: {string(RolePod), string(RoleService), string(RoleEndpointSlice)},
		RoleEndpoint:      {string(RolePod), string(RoleService), string(RoleEndpoint)},
		RoleNode:          {string(RoleNode)},
		RoleIngress:       {string(RoleIngress)},
	}

	for _, selector := range c.Selectors {
		if _, ok := foundSelectorRoles[selector.Role]; ok {
			return fmt.Errorf("duplicated selector role: %s", selector.Role)
		}
		foundSelectorRoles[selector.Role] = struct{}{}

		if _, ok := allowedSelectors[c.Role]; !ok {
			return fmt.Errorf("invalid role: %q, expecting one of: pod, service, endpoints, endpointslice, node or ingress", c.Role)
		}
		if !slices.Contains(allowedSelectors[c.Role], string(selector.Role)) {
			return fmt.Errorf("%s role supports only %s selectors", c.Role, strings.Join(allowedSelectors[c.Role], ", "))
		}
		_, err := fields.ParseSelector(selector.Field)
		if err != nil {
			return err
		}
		_, err = labels.Parse(selector.Label)
		if err != nil {
			return err
		}
	}
	return nil
}

// NamespaceDiscovery is the configuration for discovering
// Kubernetes namespaces.
type NamespaceDiscovery struct {
	IncludeOwnNamespace bool     `yaml:"own_namespace"`
	Names               []string `yaml:"names"`
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (c *NamespaceDiscovery) UnmarshalYAML(unmarshal func(interface{}) error) error {
	*c = NamespaceDiscovery{}
	type plain NamespaceDiscovery
	return unmarshal((*plain)(c))
}

// Discovery implements the discoverer interface for discovering
// targets from Kubernetes.
type Discovery struct {
	sync.RWMutex
	client             kubernetes.Interface
	role               Role
	logger             *slog.Logger
	namespaceDiscovery *NamespaceDiscovery
	discoverers        []discovery.Discoverer
	selectors          roleSelector
	ownNamespace       string
	attachMetadata     AttachMetadataConfig
	metrics            *kubernetesMetrics
}

func (d *Discovery) getNamespaces() []string {
	namespaces := d.namespaceDiscovery.Names
	includeOwnNamespace := d.namespaceDiscovery.IncludeOwnNamespace

	if len(namespaces) == 0 && !includeOwnNamespace {
		return []string{apiv1.NamespaceAll}
	}

	if includeOwnNamespace && d.ownNamespace != "" {
		return append(namespaces, d.ownNamespace)
	}

	return namespaces
}

// New creates a new Kubernetes discovery for the given role.
func New(l *slog.Logger, metrics discovery.DiscovererMetrics, conf *SDConfig) (*Discovery, error) {
	m, ok := metrics.(*kubernetesMetrics)
	if !ok {
		return nil, errors.New("invalid discovery metrics type")
	}

	if l == nil {
		l = promslog.NewNopLogger()
	}
	var (
		kcfg         *rest.Config
		err          error
		ownNamespace string
	)
	switch {
	case conf.KubeConfig != "":
		kcfg, err = clientcmd.BuildConfigFromFlags("", conf.KubeConfig)
		if err != nil {
			return nil, err
		}
	case conf.APIServer.URL == nil:
		// Use the Kubernetes provided pod service account
		// as described in https://kubernetes.io/docs/tasks/run-application/access-api-from-pod/#using-official-client-libraries
		kcfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, err
		}

		if conf.NamespaceDiscovery.IncludeOwnNamespace {
			ownNamespaceContents, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
			if err != nil {
				return nil, fmt.Errorf("could not determine the pod's namespace: %w", err)
			}
			if len(ownNamespaceContents) == 0 {
				return nil, errors.New("could not read own namespace name (empty file)")
			}
			ownNamespace = string(ownNamespaceContents)
		}

		l.Info("Using pod service account via in-cluster config")
	default:
		rt, err := config.NewRoundTripperFromConfig(conf.HTTPClientConfig, "kubernetes_sd")
		if err != nil {
			return nil, err
		}
		kcfg = &rest.Config{
			Host:      conf.APIServer.String(),
			Transport: rt,
		}
	}

	kcfg.UserAgent = version.PrometheusUserAgent()
	kcfg.ContentType = "application/vnd.kubernetes.protobuf"

	c, err := kubernetes.NewForConfig(kcfg)
	if err != nil {
		return nil, err
	}

	d := &Discovery{
		client:             c,
		logger:             l,
		role:               conf.Role,
		namespaceDiscovery: &conf.NamespaceDiscovery,
		discoverers:        make([]discovery.Discoverer, 0),
		selectors:          mapSelector(conf.Selectors),
		ownNamespace:       ownNamespace,
		attachMetadata:     conf.AttachMetadata,
		metrics:            m,
	}

	return d, nil
}

func mapSelector(rawSelector []SelectorConfig) roleSelector {
	rs := roleSelector{}
	for _, resourceSelectorRaw := range rawSelector {
		switch resourceSelectorRaw.Role {
		case RoleEndpointSlice:
			rs.endpointslice.field = resourceSelectorRaw.Field
			rs.endpointslice.label = resourceSelectorRaw.Label
		case RoleEndpoint:
			rs.endpoints.field = resourceSelectorRaw.Field
			rs.endpoints.label = resourceSelectorRaw.Label
		case RoleIngress:
			rs.ingress.field = resourceSelectorRaw.Field
			rs.ingress.label = resourceSelectorRaw.Label
		case RoleNode:
			rs.node.field = resourceSelectorRaw.Field
			rs.node.label = resourceSelectorRaw.Label
		case RolePod:
			rs.pod.field = resourceSelectorRaw.Field
			rs.pod.label = resourceSelectorRaw.Label
		case RoleService:
			rs.service.field = resourceSelectorRaw.Field
			rs.service.label = resourceSelectorRaw.Label
		}
	}
	return rs
}

// Disable the informer's resync, which just periodically resends already processed updates and distort SD metrics.
const resyncDisabled = 0

// Run implements the discoverer interface.
func (d *Discovery) Run(ctx context.Context, ch chan<- []*targetgroup.Group) {
	d.Lock()

	namespaces := d.getNamespaces()

	switch d.role {
	case RoleEndpointSlice:
		for _, namespace := range namespaces {
			var informer cache.SharedIndexInformer
			e := d.client.DiscoveryV1().EndpointSlices(namespace)
			elw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.endpointslice.field
					options.LabelSelector = d.selectors.endpointslice.label
					return e.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.endpointslice.field
					options.LabelSelector = d.selectors.endpointslice.label
					return e.Watch(ctx, options)
				},
			}
			informer = d.newIndexedEndpointSlicesInformer(elw, &disv1.EndpointSlice{})

			s := d.client.CoreV1().Services(namespace)
			slw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.service.field
					options.LabelSelector = d.selectors.service.label
					return s.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.service.field
					options.LabelSelector = d.selectors.service.label
					return s.Watch(ctx, options)
				},
			}
			p := d.client.CoreV1().Pods(namespace)
			plw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.pod.field
					options.LabelSelector = d.selectors.pod.label
					return p.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.pod.field
					options.LabelSelector = d.selectors.pod.label
					return p.Watch(ctx, options)
				},
			}
			var nodeInf cache.SharedInformer
			if d.attachMetadata.Node {
				nodeInf = d.newNodeInformer(context.Background())
				go nodeInf.Run(ctx.Done())
			}
			var namespaceInf cache.SharedInformer
			if d.attachMetadata.Namespace {
				namespaceInf = d.newNamespaceInformer(context.Background())
				go namespaceInf.Run(ctx.Done())
			}
			eps := NewEndpointSlice(
				d.logger.With("role", "endpointslice"),
				informer,
				d.mustNewSharedInformer(slw, &apiv1.Service{}, resyncDisabled),
				d.mustNewSharedInformer(plw, &apiv1.Pod{}, resyncDisabled),
				nodeInf,
				namespaceInf,
				d.metrics.eventCount,
			)
			d.discoverers = append(d.discoverers, eps)
			go eps.endpointSliceInf.Run(ctx.Done())
			go eps.serviceInf.Run(ctx.Done())
			go eps.podInf.Run(ctx.Done())
		}
	case RoleEndpoint:
		for _, namespace := range namespaces {
			e := d.client.CoreV1().Endpoints(namespace)
			elw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.endpoints.field
					options.LabelSelector = d.selectors.endpoints.label
					return e.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.endpoints.field
					options.LabelSelector = d.selectors.endpoints.label
					return e.Watch(ctx, options)
				},
			}
			s := d.client.CoreV1().Services(namespace)
			slw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.service.field
					options.LabelSelector = d.selectors.service.label
					return s.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.service.field
					options.LabelSelector = d.selectors.service.label
					return s.Watch(ctx, options)
				},
			}
			p := d.client.CoreV1().Pods(namespace)
			plw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.pod.field
					options.LabelSelector = d.selectors.pod.label
					return p.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.pod.field
					options.LabelSelector = d.selectors.pod.label
					return p.Watch(ctx, options)
				},
			}
			var nodeInf cache.SharedInformer
			if d.attachMetadata.Node {
				nodeInf = d.newNodeInformer(ctx)
				go nodeInf.Run(ctx.Done())
			}
			var namespaceInf cache.SharedInformer
			if d.attachMetadata.Namespace {
				namespaceInf = d.newNamespaceInformer(ctx)
				go namespaceInf.Run(ctx.Done())
			}

			eps := NewEndpoints(
				d.logger.With("role", "endpoint"),
				d.newIndexedEndpointsInformer(elw),
				d.mustNewSharedInformer(slw, &apiv1.Service{}, resyncDisabled),
				d.mustNewSharedInformer(plw, &apiv1.Pod{}, resyncDisabled),
				nodeInf,
				namespaceInf,
				d.metrics.eventCount,
			)
			d.discoverers = append(d.discoverers, eps)
			go eps.endpointsInf.Run(ctx.Done())
			go eps.serviceInf.Run(ctx.Done())
			go eps.podInf.Run(ctx.Done())
		}
	case RolePod:
		var nodeInformer cache.SharedInformer
		if d.attachMetadata.Node {
			nodeInformer = d.newNodeInformer(ctx)
			go nodeInformer.Run(ctx.Done())
		}
		var namespaceInformer cache.SharedInformer
		if d.attachMetadata.Namespace {
			namespaceInformer = d.newNamespaceInformer(ctx)
			go namespaceInformer.Run(ctx.Done())
		}

		for _, namespace := range namespaces {
			p := d.client.CoreV1().Pods(namespace)
			plw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.pod.field
					options.LabelSelector = d.selectors.pod.label
					return p.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.pod.field
					options.LabelSelector = d.selectors.pod.label
					return p.Watch(ctx, options)
				},
			}
			pod := NewPod(
				d.logger.With("role", "pod"),
				d.newIndexedPodsInformer(plw),
				nodeInformer,
				namespaceInformer,
				d.metrics.eventCount,
			)
			d.discoverers = append(d.discoverers, pod)
			go pod.podInf.Run(ctx.Done())
		}
	case RoleService:
		var namespaceInformer cache.SharedInformer
		if d.attachMetadata.Namespace {
			namespaceInformer = d.newNamespaceInformer(ctx)
			go namespaceInformer.Run(ctx.Done())
		}

		for _, namespace := range namespaces {
			s := d.client.CoreV1().Services(namespace)
			slw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.service.field
					options.LabelSelector = d.selectors.service.label
					return s.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.service.field
					options.LabelSelector = d.selectors.service.label
					return s.Watch(ctx, options)
				},
			}
			svc := NewService(
				d.logger.With("role", "service"),
				d.newIndexedServicesInformer(slw),
				namespaceInformer,
				d.metrics.eventCount,
			)
			d.discoverers = append(d.discoverers, svc)
			go svc.informer.Run(ctx.Done())
		}
	case RoleIngress:
		var namespaceInformer cache.SharedInformer
		if d.attachMetadata.Namespace {
			namespaceInformer = d.newNamespaceInformer(ctx)
			go namespaceInformer.Run(ctx.Done())
		}

		for _, namespace := range namespaces {
			i := d.client.NetworkingV1().Ingresses(namespace)
			ilw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = d.selectors.ingress.field
					options.LabelSelector = d.selectors.ingress.label
					return i.List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = d.selectors.ingress.field
					options.LabelSelector = d.selectors.ingress.label
					return i.Watch(ctx, options)
				},
			}
			ingress := NewIngress(
				d.logger.With("role", "ingress"),
				d.newIndexedIngressesInformer(ilw),
				namespaceInformer,
				d.metrics.eventCount,
			)
			d.discoverers = append(d.discoverers, ingress)
			go ingress.informer.Run(ctx.Done())
		}
	case RoleNode:
		nodeInformer := d.newNodeInformer(ctx)
		node := NewNode(d.logger.With("role", "node"), nodeInformer, d.metrics.eventCount)
		d.discoverers = append(d.discoverers, node)
		go node.informer.Run(ctx.Done())
	default:
		d.logger.Error("unknown Kubernetes discovery kind", "role", d.role)
	}

	var wg sync.WaitGroup
	for _, dd := range d.discoverers {
		wg.Add(1)
		go func(d discovery.Discoverer) {
			defer wg.Done()
			d.Run(ctx, ch)
		}(dd)
	}

	d.Unlock()

	wg.Wait()
	<-ctx.Done()
}

func lv(s string) model.LabelValue {
	return model.LabelValue(s)
}

func send(ctx context.Context, ch chan<- []*targetgroup.Group, tg *targetgroup.Group) {
	if tg == nil {
		return
	}
	select {
	case <-ctx.Done():
	case ch <- []*targetgroup.Group{tg}:
	}
}

func retryOnError(ctx context.Context, interval time.Duration, f func() error) (canceled bool) {
	var err error
	err = f()
	for {
		if err == nil {
			return false
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(interval):
			err = f()
		}
	}
}

func (d *Discovery) newNodeInformer(ctx context.Context) cache.SharedInformer {
	nlw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = d.selectors.node.field
			options.LabelSelector = d.selectors.node.label
			return d.client.CoreV1().Nodes().List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = d.selectors.node.field
			options.LabelSelector = d.selectors.node.label
			return d.client.CoreV1().Nodes().Watch(ctx, options)
		},
	}
	return d.mustNewSharedInformer(nlw, &apiv1.Node{}, resyncDisabled)
}

func (d *Discovery) newNamespaceInformer(ctx context.Context) cache.SharedInformer {
	// We don't filter on NamespaceDiscovery.
	nlw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return d.client.CoreV1().Namespaces().List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return d.client.CoreV1().Namespaces().Watch(ctx, options)
		},
	}
	return d.mustNewSharedInformer(nlw, &apiv1.Namespace{}, resyncDisabled)
}

func (d *Discovery) newIndexedPodsInformer(plw *cache.ListWatch) cache.SharedIndexInformer {
	indexers := make(map[string]cache.IndexFunc)
	if d.attachMetadata.Node {
		indexers[nodeIndex] = func(obj interface{}) ([]string, error) {
			pod, ok := obj.(*apiv1.Pod)
			if !ok {
				return nil, errors.New("object is not a pod")
			}
			return []string{pod.Spec.NodeName}, nil
		}
	}

	if d.attachMetadata.Namespace {
		indexers[cache.NamespaceIndex] = cache.MetaNamespaceIndexFunc
	}

	return d.mustNewSharedIndexInformer(plw, &apiv1.Pod{}, resyncDisabled, indexers)
}

func (d *Discovery) newIndexedEndpointsInformer(plw *cache.ListWatch) cache.SharedIndexInformer {
	indexers := make(map[string]cache.IndexFunc)
	indexers[podIndex] = func(obj interface{}) ([]string, error) {
		e, ok := obj.(*apiv1.Endpoints)
		if !ok {
			return nil, errors.New("object is not endpoints")
		}
		var pods []string
		for _, target := range e.Subsets {
			for _, addr := range target.Addresses {
				if addr.TargetRef != nil && addr.TargetRef.Kind == "Pod" {
					pods = append(pods, namespacedName(addr.TargetRef.Namespace, addr.TargetRef.Name))
				}
			}
		}
		return pods, nil
	}

	if d.attachMetadata.Node {
		indexers[nodeIndex] = func(obj interface{}) ([]string, error) {
			e, ok := obj.(*apiv1.Endpoints)
			if !ok {
				return nil, errors.New("object is not endpoints")
			}
			var nodes []string
			for _, target := range e.Subsets {
				for _, addr := range target.Addresses {
					if addr.TargetRef != nil {
						switch addr.TargetRef.Kind {
						case "Pod":
							if addr.NodeName != nil {
								nodes = append(nodes, *addr.NodeName)
							}
						case "Node":
							nodes = append(nodes, addr.TargetRef.Name)
						}
					}
				}
			}
			return nodes, nil
		}
	}

	if d.attachMetadata.Namespace {
		indexers[cache.NamespaceIndex] = cache.MetaNamespaceIndexFunc
	}

	return d.mustNewSharedIndexInformer(plw, &apiv1.Endpoints{}, resyncDisabled, indexers)
}

func (d *Discovery) newIndexedEndpointSlicesInformer(plw *cache.ListWatch, object runtime.Object) cache.SharedIndexInformer {
	indexers := make(map[string]cache.IndexFunc)
	indexers[serviceIndex] = func(obj interface{}) ([]string, error) {
		e, ok := obj.(*disv1.EndpointSlice)
		if !ok {
			return nil, errors.New("object is not an endpointslice")
		}

		svcName, exists := e.Labels[disv1.LabelServiceName]
		if !exists {
			return nil, nil
		}

		return []string{namespacedName(e.Namespace, svcName)}, nil
	}

	if d.attachMetadata.Node {
		indexers[nodeIndex] = func(obj interface{}) ([]string, error) {
			e, ok := obj.(*disv1.EndpointSlice)
			if !ok {
				return nil, errors.New("object is not an endpointslice")
			}

			var nodes []string
			for _, target := range e.Endpoints {
				if target.TargetRef != nil {
					switch target.TargetRef.Kind {
					case "Pod":
						if target.NodeName != nil {
							nodes = append(nodes, *target.NodeName)
						}
					case "Node":
						nodes = append(nodes, target.TargetRef.Name)
					}
				}
			}

			return nodes, nil
		}
	}

	if d.attachMetadata.Namespace {
		indexers[cache.NamespaceIndex] = cache.MetaNamespaceIndexFunc
	}

	return d.mustNewSharedIndexInformer(plw, object, resyncDisabled, indexers)
}

func (d *Discovery) newIndexedServicesInformer(slw *cache.ListWatch) cache.SharedIndexInformer {
	indexers := make(map[string]cache.IndexFunc)

	if d.attachMetadata.Namespace {
		indexers[cache.NamespaceIndex] = cache.MetaNamespaceIndexFunc
	}

	return d.mustNewSharedIndexInformer(slw, &apiv1.Service{}, resyncDisabled, indexers)
}

func (d *Discovery) newIndexedIngressesInformer(ilw *cache.ListWatch) cache.SharedIndexInformer {
	indexers := make(map[string]cache.IndexFunc)

	if d.attachMetadata.Namespace {
		indexers[cache.NamespaceIndex] = cache.MetaNamespaceIndexFunc
	}

	return d.mustNewSharedIndexInformer(ilw, &networkv1.Ingress{}, resyncDisabled, indexers)
}

func (d *Discovery) informerWatchErrorHandler(r *cache.Reflector, err error) {
	d.metrics.failuresCount.Inc()
	cache.DefaultWatchErrorHandler(r, err)
}

func (d *Discovery) mustNewSharedInformer(lw cache.ListerWatcher, exampleObject runtime.Object, defaultEventHandlerResyncPeriod time.Duration) cache.SharedInformer {
	informer := cache.NewSharedInformer(lw, exampleObject, defaultEventHandlerResyncPeriod)
	// Invoking SetWatchErrorHandler should fail only if the informer has been started beforehand.
	// Such a scenario would suggest an incorrect use of the API, thus the panic.
	if err := informer.SetWatchErrorHandler(d.informerWatchErrorHandler); err != nil {
		panic(err)
	}
	return informer
}

func (d *Discovery) mustNewSharedIndexInformer(lw cache.ListerWatcher, exampleObject runtime.Object, defaultEventHandlerResyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	informer := cache.NewSharedIndexInformer(lw, exampleObject, defaultEventHandlerResyncPeriod, indexers)
	// Invoking SetWatchErrorHandler should fail only if the informer has been started beforehand.
	// Such a scenario would suggest an incorrect use of the API, thus the panic.
	if err := informer.SetWatchErrorHandler(d.informerWatchErrorHandler); err != nil {
		panic(err)
	}
	return informer
}

func addObjectAnnotationsAndLabels(labelSet model.LabelSet, objectMeta metav1.ObjectMeta, resource string) {
	for k, v := range objectMeta.Labels {
		ln := strutil.SanitizeLabelName(k)
		labelSet[model.LabelName(metaLabelPrefix+resource+"_label_"+ln)] = lv(v)
		labelSet[model.LabelName(metaLabelPrefix+resource+"_labelpresent_"+ln)] = presentValue
	}
	for k, v := range objectMeta.Annotations {
		ln := strutil.SanitizeLabelName(k)
		labelSet[model.LabelName(metaLabelPrefix+resource+"_annotation_"+ln)] = lv(v)
		labelSet[model.LabelName(metaLabelPrefix+resource+"_annotationpresent_"+ln)] = presentValue
	}
}

func addObjectMetaLabels(labelSet model.LabelSet, objectMeta metav1.ObjectMeta, role Role) {
	labelSet[model.LabelName(metaLabelPrefix+string(role)+"_name")] = lv(objectMeta.Name)
	addObjectAnnotationsAndLabels(labelSet, objectMeta, string(role))
}

func addNamespaceMetaLabels(labelSet model.LabelSet, objectMeta metav1.ObjectMeta) {
	// Omitting the namespace name because should be already injected elsewhere.
	addObjectAnnotationsAndLabels(labelSet, objectMeta, "namespace")
}

func namespacedName(namespace, name string) string {
	return namespace + "/" + name
}

// nodeName knows how to handle the cache.DeletedFinalStateUnknown tombstone.
// It assumes the MetaNamespaceKeyFunc keyFunc is used, which uses the node name as the tombstone key.
func nodeName(o interface{}) (string, error) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(o)
	if err != nil {
		return "", err
	}
	return key, nil
}
