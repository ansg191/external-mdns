package source

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/blake/external-mdns/resource"
	"github.com/jpillora/go-tld"
	informers "github.com/traefik/traefik/v3/pkg/provider/kubernetes/crd/generated/informers/externalversions"
	traefikio "github.com/traefik/traefik/v3/pkg/provider/kubernetes/crd/traefikio/v1alpha1"
	"github.com/traefik/traefik/v3/pkg/rules"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

var matchers = []string{
	"ClientIP",
	"Method",
	"Host",
	"HostRegexp",
	"Path",
	"PathRegexp",
	"PathPrefix",
	"Header",
	"Headers",
	"HeaderRegexp",
	"Query",
	"QueryRegexp",
}

// TraefikIngressRouteSource handles adding, updating, or removing mDNS record advertisements
type TraefikIngressRouteSource struct {
	namespace      string
	notifyChan     chan<- resource.Resource
	sharedInformer cache.SharedIndexInformer
	dstIPAddr      []net.IP
}

func (s *TraefikIngressRouteSource) Run(stopCh chan struct{}) error {
	s.sharedInformer.Run(stopCh)
	if !cache.WaitForCacheSync(stopCh, s.sharedInformer.HasSynced) {
		runtime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
	}
	return nil
}

func (s *TraefikIngressRouteSource) onAdd(obj interface{}) {
	advertiseRecords, err := s.buildRecords(obj, resource.Added)
	if err != nil {
		fmt.Println("Error adding Traefik ingress route", err)
		return
	}

	for _, record := range advertiseRecords {
		s.notifyChan <- record
	}
}

func (s *TraefikIngressRouteSource) onDelete(obj interface{}) {
	advertiseRecords, err := s.buildRecords(obj, resource.Deleted)
	if err != nil {
		fmt.Println("Error deleting Traefik ingress route", err)
		return
	}

	for _, record := range advertiseRecords {
		s.notifyChan <- record
	}
}

func (s *TraefikIngressRouteSource) onUpdate(oldObj, newObj interface{}) {
	oldResources, err1 := s.buildRecords(oldObj, resource.Updated)
	if err1 != nil {
		fmt.Printf("Error gathering old ingress resources: %s", err1)
	}

	for _, record := range oldResources {
		record.Action = resource.Deleted
		s.notifyChan <- record
	}

	newResources, err2 := s.buildRecords(newObj, resource.Updated)
	if err2 != nil {
		fmt.Printf("Error gathering new ingress resources: %s", err2)
	}

	for _, record := range newResources {
		record.Action = resource.Added
		s.notifyChan <- record
	}
}

func (s *TraefikIngressRouteSource) buildRecords(obj interface{}, action string) ([]resource.Resource, error) {
	var records []resource.Resource

	ingress, ok := obj.(*traefikio.IngressRoute)
	if !ok {
		return records, nil
	}

	var ipAddrs []string
	for _, ip := range s.dstIPAddr {
		if len(ip) != 0 {
			ipAddrs = append(ipAddrs, ip.String())
		}
	}

	parser, err := rules.NewParser(matchers)
	if err != nil {
		return nil, err
	}

	var hostname string
	for _, route := range ingress.Spec.Routes {
		parsed, err := parser.Parse(route.Match)
		if err != nil {
			return nil, err
		}

		treeBuilder, ok := parsed.(rules.TreeBuilder)
		if !ok {
			return nil, fmt.Errorf("unable to parse match rule")
		}

		tree := treeBuilder()
		hosts, err := extractHosts(tree)
		if err != nil {
			return nil, err
		}

		for _, host := range hosts {
			// Skip hostnames that do not use .local
			if !strings.HasSuffix(host, ".local") {
				continue
			}

			fakeURL := fmt.Sprintf("http://%s", host)
			parsedHost, err := tld.Parse(fakeURL)

			if err != nil {
				log.Printf("Unable to parse hostname %s. %s", host, err.Error())
				continue
			}

			if parsedHost.Subdomain != "" {
				hostname = fmt.Sprintf("%s.%s", parsedHost.Subdomain, parsedHost.Domain)
			} else {
				hostname = parsedHost.Domain
			}
			advertiseObj := resource.Resource{
				SourceType: "ingress",
				Action:     action,
				Name:       hostname,
				Namespace:  ingress.Namespace,
				IPs:        ipAddrs,
			}

			records = append(records, advertiseObj)
		}
	}

	return records, nil
}

func NewTraefikIngressRouteWatcher(
	client kubernetes.Interface,
	factory informers.SharedInformerFactory,
	namespace string,
	notifyChan chan<- resource.Resource,
) TraefikIngressRouteSource {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get the Traefik service IP addresses
	dstIPs, err := getDstIPAddr(ctx, client)
	if err != nil {
		log.Fatalf("Failed to get Traefik service IP addresses: %v", err)
	}
	log.Printf("Traefik service IP addresses: %v", dstIPs)

	ingressInformer := factory.Traefik().V1alpha1().IngressRoutes().Informer()
	i := &TraefikIngressRouteSource{
		namespace:      namespace,
		notifyChan:     notifyChan,
		sharedInformer: ingressInformer,
		dstIPAddr:      dstIPs,
	}

	_, _ = ingressInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    i.onAdd,
		UpdateFunc: i.onUpdate,
		DeleteFunc: i.onDelete,
	})

	return *i
}

func getDstIPAddr(ctx context.Context, client kubernetes.Interface) ([]net.IP, error) {
	// Find the Traefik service
	services, err := client.CoreV1().Services("").List(ctx, v1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var dstIPs []net.IP
	for _, service := range services.Items {
		if service.Labels["app.kubernetes.io/name"] != "traefik" {
			continue
		}

		// Check if the service has a LoadBalancer
		if service.Spec.Type != "LoadBalancer" {
			continue
		}

		// Get the LoadBalancer IPs
		for _, ingress := range service.Status.LoadBalancer.Ingress {
			ip := net.ParseIP(ingress.IP)
			if ip != nil {
				dstIPs = append(dstIPs, ip)
			} else {
				log.Printf("Unable to parse IP address %s", ingress.IP)
			}
		}
	}

	return dstIPs, nil
}

func extractHosts(tree *rules.Tree) ([]string, error) {
	var hosts []string

	if tree.RuleLeft != nil {
		newHosts, err := extractHosts(tree.RuleLeft)
		if err != nil {
			return hosts, err
		}
		hosts = append(hosts, newHosts...)
	}
	if tree.RuleRight != nil {
		newHosts, err := extractHosts(tree.RuleRight)
		if err != nil {
			return hosts, err
		}
		hosts = append(hosts, newHosts...)
	}

	// Skip negation
	if tree.Not {
		return hosts, nil
	}

	switch tree.Matcher {
	case "Host":
		hosts = append(hosts, tree.Value[0])
	case "HostRegexp":
		return hosts, fmt.Errorf("HostRegexp not supported")
	default:
		// Do nothing
	}

	return hosts, nil
}
