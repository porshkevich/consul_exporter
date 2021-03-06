package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	consul_api "github.com/hashicorp/consul/api"
)

const (
	namespace = "consul"
)

var (
	up = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "up"),
		"Was the last query of Consul successful.",
		nil, nil,
	)
	clusterServers = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "raft_peers"),
		"How many peers (servers) are in the Raft cluster.",
		nil, nil,
	)
	clusterLeader = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "raft_leader"),
		"Does Raft cluster have a leader (according to this node).",
		nil, nil,
	)
	nodeCount = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "serf_lan_members"),
		"How many members are in the cluster.",
		[]string{"datacenter"}, nil,
	)
	serviceCount = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "catalog_services"),
		"How many services are in the cluster.",
		[]string{"datacenter"}, nil,
	)
	serviceTag = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "service_tag"),
		"Tags of a service.",
		[]string{"service_id", "node", "tag"}, nil,
	)
	serviceNodesHealthy = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "catalog_service_node_healthy"),
		"Is this service healthy on this node?",
		[]string{"service_id", "node", "service_name", "datacenter", "tags"}, nil,
	)
	nodeChecks = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "health_node_status"),
		"Status of health checks associated with a node.",
		[]string{"check", "node", "status", "datacenter"}, nil,
	)
	serviceChecks = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "health_service_status"),
		"Status of health checks associated with a service.",
		[]string{"check", "node", "service_id", "service_name", "status", "datacenter", "tags"}, nil,
	)
	keyValues = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "catalog_kv"),
		"The values for selected keys in Consul's key/value catalog. Keys with non-numeric values are omitted.",
		[]string{"key"}, nil,
	)
	queryOptions = consul_api.QueryOptions{}
)

// Exporter collects Consul stats from the given server and exports them using
// the prometheus metrics package.
type Exporter struct {
	client        *consul_api.Client
	kvPrefix      string
	kvFilter      *regexp.Regexp
	healthSummary bool
}

type consulOpts struct {
	uri        string
	caFile     string
	certFile   string
	keyFile    string
	serverName string
	timeout    time.Duration
}

// NewExporter returns an initialized Exporter.
func NewExporter(opts consulOpts, kvPrefix, kvFilter string, healthSummary bool) (*Exporter, error) {
	uri := opts.uri
	if !strings.Contains(uri, "://") {
		uri = "http://" + uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid consul URL: %s", err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid consul URL: %s", uri)
	}

	tlsConfig := consul_api.TLSConfig{
		Address:  opts.serverName,
		CAFile:   opts.caFile,
		CertFile: opts.certFile,
		KeyFile:  opts.keyFile,
	}
	config := consul_api.DefaultConfig()
	config.Address = u.Host
	config.Scheme = u.Scheme
	config.TLSConfig = tlsConfig
        config.HttpClient, err = consul_api.NewHttpClient(config.Transport, config.TLSConfig)
	config.HttpClient.Timeout = opts.timeout

	client, err := consul_api.NewClient(config)
	if err != nil {
		return nil, err
	}

	// Init our exporter.
	return &Exporter{
		client:        client,
		kvPrefix:      kvPrefix,
		kvFilter:      regexp.MustCompile(kvFilter),
		healthSummary: healthSummary,
	}, nil
}

// Describe describes all the metrics ever exported by the Consul exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- up
	ch <- clusterServers
	ch <- clusterLeader
	ch <- nodeCount
	ch <- serviceCount
	ch <- serviceNodesHealthy
	ch <- nodeChecks
	ch <- serviceChecks
	ch <- keyValues
	ch <- serviceTag
}

// Collect fetches the stats from configured Consul location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	// How many peers are in the Consul cluster?
	peers, err := e.client.Status().Peers()
	if err != nil {
		ch <- prometheus.MustNewConstMetric(
			up, prometheus.GaugeValue, 0,
		)
		log.Errorf("Can't query consul: %v", err)
		return
	}

	// We'll use peers to decide that we're up.
	ch <- prometheus.MustNewConstMetric(
		up, prometheus.GaugeValue, 1,
	)
	ch <- prometheus.MustNewConstMetric(
		clusterServers, prometheus.GaugeValue, float64(len(peers)),
	)

	leader, err := e.client.Status().Leader()
	if err != nil {
		log.Errorf("Can't query consul: %v", err)
	}
	if len(leader) == 0 {
		ch <- prometheus.MustNewConstMetric(
			clusterLeader, prometheus.GaugeValue, 0,
		)
	} else {
		ch <- prometheus.MustNewConstMetric(
			clusterLeader, prometheus.GaugeValue, 1,
		)
	}

	datacenters, err := e.client.Catalog().Datacenters()
	if err != nil {
		c, _ := e.client.Agent().Self()
		datacenters = []string{c["Config"]["Datacenter"].(string)}
	}

	e.collectByDatacenter(ch, datacenters)

	e.collectKeyValues(ch)
}

// collectHealthSummary collects health information about every node+service
// combination. It will cause one lookup query per service.
func (e *Exporter) collectByDatacenter(ch chan<- prometheus.Metric, datacenters []string) {
	var wg sync.WaitGroup

	for _, s := range datacenters {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()

			var queryOptions = queryOptions
			queryOptions.Datacenter = s
			// How many nodes are registered?
			nodes, _, err := e.client.Catalog().Nodes(&queryOptions)
			if err != nil {
				// FIXME: How should we handle a partial failure like this?
			} else {
				ch <- prometheus.MustNewConstMetric(
					nodeCount, prometheus.GaugeValue, float64(len(nodes)), queryOptions.Datacenter,
				)
			}

			// Query for the full list of services.
			serviceNames, _, err := e.client.Catalog().Services(&queryOptions)
			if err != nil {
				// FIXME: How should we handle a partial failure like this?
				return
			}
			ch <- prometheus.MustNewConstMetric(
				serviceCount, prometheus.GaugeValue, float64(len(serviceNames)), queryOptions.Datacenter,
			)

			if e.healthSummary {
				e.collectHealthSummary(ch, serviceNames, &queryOptions)
			}

			checks, _, err := e.client.Health().State("any", &queryOptions)
			if err != nil {
				log.Errorf("Failed to query service health: %v", err)
				return
			}

			for _, hc := range checks {
				var status float64

				switch hc.Status {
				case consul_api.HealthPassing:
					status = 1
				case consul_api.HealthWarning:
					status = 2
				case consul_api.HealthCritical:
					status = 3
				case consul_api.HealthMaint:
					status = 0
				}

				if hc.ServiceID == "" {
					ch <- prometheus.MustNewConstMetric(
						nodeChecks, prometheus.GaugeValue, status, hc.CheckID, hc.Node, hc.Status, queryOptions.Datacenter,
					)
				} else {
					ch <- prometheus.MustNewConstMetric(
						serviceChecks, prometheus.GaugeValue, status, hc.CheckID, hc.Node, hc.ServiceID, hc.ServiceName, queryOptions.Datacenter, hc.Status, "," + strings.Join(hc.ServiceTags, ",") + ",",
					)
				}
			}
		}(s)
	}

	wg.Wait()
}

// collectHealthSummary collects health information about every node+service
// combination. It will cause one lookup query per service.
func (e *Exporter) collectHealthSummary(ch chan<- prometheus.Metric, serviceNames map[string][]string, queryOptions *consul_api.QueryOptions) {
	var wg sync.WaitGroup

	for s := range serviceNames {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			e.collectOneHealthSummary(ch, s, queryOptions)
		}(s)
	}

	wg.Wait()
}

func (e *Exporter) collectOneHealthSummary(ch chan<- prometheus.Metric, serviceName string, queryOptions *consul_api.QueryOptions) error {
	log.Debugf("Fetching health summary for: %s", serviceName)

	service, _, err := e.client.Health().Service(serviceName, "", false, queryOptions)
	if err != nil {
		log.Errorf("Failed to query service health: %v", err)
		return err
	}

	for _, entry := range service {
		// We have a Node, a Service, and one or more Checks. Our
		// service-node combo is passing if all checks have a `status`
		// of "passing."
		var status float64

		switch entry.Checks.AggregatedStatus() {
		case consul_api.HealthPassing:
			status = 1
		case consul_api.HealthWarning:
			status = 2
		case consul_api.HealthCritical:
			status = 3
		case consul_api.HealthMaint:
			status = 0
		}
		ch <- prometheus.MustNewConstMetric(
			serviceNodesHealthy, prometheus.GaugeValue, status, entry.Service.ID, entry.Node.Node, entry.Service.Service, queryOptions.Datacenter, ","+strings.Join(entry.Service.Tags, ",")+",",
		)
	}
	return nil
}

func (e *Exporter) collectKeyValues(ch chan<- prometheus.Metric) {
	if e.kvPrefix == "" {
		return
	}

	kv := e.client.KV()
	pairs, _, err := kv.List(e.kvPrefix, &queryOptions)
	if err != nil {
		log.Errorf("Error fetching key/values: %s", err)
		return
	}

	for _, pair := range pairs {
		if e.kvFilter.MatchString(pair.Key) {
			val, err := strconv.ParseFloat(string(pair.Value), 64)
			if err == nil {
				ch <- prometheus.MustNewConstMetric(
					keyValues, prometheus.GaugeValue, val, pair.Key,
				)
			}
		}
	}
}

func init() {
	prometheus.MustRegister(version.NewCollector("consul_exporter"))
}

func main() {
	var (
		listenAddress = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9107").String()
		metricsPath   = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		healthSummary = kingpin.Flag("consul.health-summary", "Generate a health summary for each service instance. Needs n+1 queries to collect all information.").Default("true").Bool()
		kvPrefix      = kingpin.Flag("kv.prefix", "Prefix from which to expose key/value pairs.").Default("").String()
		kvFilter      = kingpin.Flag("kv.filter", "Regex that determines which keys to expose.").Default(".*").String()

		opts = consulOpts{}
	)
	kingpin.Flag("consul.server", "HTTP API address of a Consul server or agent. (prefix with https:// to connect over HTTPS)").Default("http://localhost:8500").StringVar(&opts.uri)
	kingpin.Flag("consul.ca-file", "File path to a PEM-encoded certificate authority used to validate the authenticity of a server certificate.").Default("").StringVar(&opts.caFile)
	kingpin.Flag("consul.cert-file", "File path to a PEM-encoded certificate used with the private key to verify the exporter's authenticity.").Default("").StringVar(&opts.certFile)
	kingpin.Flag("consul.key-file", "File path to a PEM-encoded private key used with the certificate to verify the exporter's authenticity.").Default("").StringVar(&opts.keyFile)
	kingpin.Flag("consul.server-name", "When provided, this overrides the hostname for the TLS certificate. It can be used to ensure that the certificate name matches the hostname we declare.").Default("").StringVar(&opts.serverName)
	kingpin.Flag("consul.timeout", "Timeout on HTTP requests to consul.").Default("200ms").DurationVar(&opts.timeout)

	// Query options.
	kingpin.Flag("consul.allow_stale", "Allows any Consul server (non-leader) to service a read.").Default("true").BoolVar(&queryOptions.AllowStale)
	kingpin.Flag("consul.require_consistent", "Forces the read to be fully consistent.").Default("false").BoolVar(&queryOptions.RequireConsistent)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("consul_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Infoln("Starting consul_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	exporter, err := NewExporter(opts, *kvPrefix, *kvFilter, *healthSummary)
	if err != nil {
		log.Fatalln(err)
	}
	prometheus.MustRegister(exporter)

	queryOptionsJson, err := json.Marshal(queryOptions)
	if err != nil {
		log.Fatalln(err)
	}

	http.Handle(*metricsPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Consul Exporter</title></head>
             <body>
             <h1>Consul Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             <h2>Options</h2>
             <pre>` + string(queryOptionsJson) + `</pre>
             </dl>
             <h2>Build</h2>
             <pre>` + version.Info() + ` ` + version.BuildContext() + `</pre>
             </body>
             </html>`))
	})

	log.Infoln("Listening on", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
