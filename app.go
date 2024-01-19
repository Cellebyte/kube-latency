package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/dustin/randbo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"github.com/tcnksm/go-httpstat"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const EnvKubeNamespace = "KUBE_NAMESPACE"
const EnvKubePodName = "KUBE_POD_NAME"

type App struct {
	rand io.Reader

	kubeClient        *kubernetes.Clientset
	kubeNamespace     string
	kubePodName       string
	kubeServiceName   string
	kubeClusterDomain string

	myService *v1.Service

	zonePerNode map[string]string

	metricDownloadProbeSize *prometheus.GaugeVec
	metricDownloadDurations *prometheus.SummaryVec
	metricPingDurations     *prometheus.SummaryVec
	metricHTTPDurations     *prometheus.SummaryVec

	metricDNSLookupDurations        *prometheus.SummaryVec
	metricTCPConnectionDurations    *prometheus.SummaryVec
	metricServerProcessingDurations *prometheus.SummaryVec

	metricStartTransferDurations *prometheus.SummaryVec
}

type Labels struct {
	PodName         string
	PodFQDN         string
	PodPort         uint16
	PodPortName     string
	PodPortProtocol string
	NodeName        string
	Zone            string
}

func LabelsKeys(prefix string) []string {
	return []string{
		prefix + "pod",
		prefix + "pod_port_name",
		prefix + "pod_port_protocol",
		prefix + "zone",
		prefix + "node_name",
	}
}
func (l *Labels) Values() []string {
	return []string{
		l.PodName,
		l.PodPortName,
		l.PodPortProtocol,
		l.Zone,
		l.NodeName,
	}
}

func NewApp() *App {
	var labels []string
	labels = append(labels, LabelsKeys("source_")...)
	labels = append(labels, LabelsKeys("dest_")...)
	return &App{
		rand: randbo.New(),
		metricDownloadProbeSize: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "download_probe_size",
				Help: "Download probe sizes in bytes",
			},
			labels,
		),
		metricDownloadDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "download_durations_seconds",
				Help:       "Download durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		metricPingDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "ping_durations_seconds",
				Help:       "Ping durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		metricHTTPDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "http_durations_seconds",
				Help:       "HTTP content-transfer durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),

		metricDNSLookupDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "dns_query_durations_seconds",
				Help:       "DNS query durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		metricTCPConnectionDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "tcp_connect_durations_seconds",
				Help:       "TCP connect durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		metricStartTransferDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "http_connect_durations_seconds",
				Help:       "HTTP connect durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		metricServerProcessingDurations: prometheus.NewSummaryVec(
			prometheus.SummaryOpts{
				Name:       "http_server_durations_seconds",
				Help:       "HTTP server processing durations in seconds",
				Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			},
			labels,
		),
		zonePerNode: map[string]string{},
	}
}

func (a *App) handlePing(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "pong")
}

func (a *App) handleData(w http.ResponseWriter, r *http.Request) {
	_, err := io.CopyN(w, a.rand, int64(*dataSize))
	if err != nil {
		log.Warn("failed to write random data: ", err)
	}
}

// get zone of node and cache in hashmap, don't cache not found nodes
func (a *App) getZoneForNode(nodeName string) string {
	if zone, ok := a.zonePerNode[nodeName]; ok {
		return zone
	}

	node, err := a.kubeClient.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		log.Warnf("error getting node %s: %s", nodeName, err)
		return ""
	}

	zone, _ := node.Labels[v1.LabelZoneFailureDomain]
	a.zonePerNode[nodeName] = zone

	return zone
}

func (a *App) testLoop() {
	for {
		// get latest pod list
		var serviceLabels labels.Set = a.myService.Spec.Selector
		podsList, err := a.kubeClient.CoreV1().Pods(a.kubeNamespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: serviceLabels.AsSelector().String(),
		})
		if err != nil {
			log.Warnf("failed to list pods with selector '%s': %s", serviceLabels.AsSelector().String(), err)
		}
		var discoveryFQDN = fmt.Sprintf("%s.%s.svc.%s", a.myService.Name, a.kubeNamespace, a.kubeClusterDomain)
		// get latest dns srv record information
		portName := a.myService.Spec.Ports[len(a.myService.Spec.Ports)-1].Name
		portProtocol := strings.ToLower(string(a.myService.Spec.Ports[len(a.myService.Spec.Ports)-1].Protocol))
		// get my SRV Record from the service discovery
		cname, srvs, err := net.LookupSRV(portName, portProtocol, discoveryFQDN)
		if err != nil {
			log.Warnf("failed to query the service SRV discovery FQDN '%s': %s", discoveryFQDN, err)
		}
		log.Infof("Discovered endpoints [%d] on %s", len(srvs), cname)

		destinations := []*Labels{}
		var source *Labels = nil

		// build labels per pod
		for _, pod := range podsList.Items {
			podIP := net.ParseIP(pod.Status.PodIP)
			if podIP == nil {
				continue
			}
			// find RDNS entries for the current pod
			ptrRecords, err := net.LookupAddr(podIP.String())
			if err != nil {
				log.Warnf("no RDNS entry for podIP %s: %v", podIP.String(), err)
				continue
			}
			if slices.Contains(ptrRecords, a.kubePodName) {
				log.Debugf("my srv Record is %s out of my rdns ptrRecords %v", a.kubePodName, ptrRecords)
				source = &Labels{
					Zone:            a.getZoneForNode(pod.Spec.NodeName),
					PodName:         pod.Name,
					PodFQDN:         "",
					PodPort:         8080,
					PodPortProtocol: portProtocol,
					PodPortName:     portName,
					NodeName:        pod.Spec.NodeName,
				}
			} else {
				// get SRV Record from the service discovery
				var labels *Labels = nil
				for _, srv := range srvs {
					if slices.Contains(ptrRecords, srv.Target) {
						log.Debugf("their srv Record is %s out of their rdns ptrRecords %v", srv.Target, ptrRecords)
						labels = &Labels{
							Zone:            a.getZoneForNode(pod.Spec.NodeName),
							PodName:         pod.Name,
							PodFQDN:         srv.Target,
							PodPort:         srv.Port,
							PodPortProtocol: portProtocol,
							PodPortName:     portName,
							NodeName:        pod.Spec.NodeName,
						}
						destinations = append(destinations, labels)
					}
				}
			}
		}
		// run tests if pods found
		if source == nil || len(destinations) == 0 {
			log.Info("skip tests no suitable pods found")
		} else {
			// run ping tests against any node
			for _, dest := range destinations {
				go a.testPing(source, dest)
			}

			// run download tests against a single random selected node
			dest := destinations[rand.Intn(len(destinations))]
			go a.testDownload(source, dest)

		}

		time.Sleep(time.Duration(*testFrequency) * time.Second)
	}
}

func (a *App) Run() {
	log.Infof("starting kube-latency v%s (git %s, %s)", AppVersion, AppGitCommit, AppGitState)

	// parse cli flags
	flag.Parse()

	// get environment variables
	a.kubeNamespace = os.Getenv(EnvKubeNamespace)
	if a.kubeNamespace == "" {
		log.Fatalf("please specify %s", EnvKubeNamespace)
	}
	a.kubePodName = os.Getenv(EnvKubePodName)
	if a.kubePodName == "" {
		log.Fatalf("please specify %s", EnvKubePodName)
	}
	// initialize the clusterDomain
	a.kubeClusterDomain = *clusterDomain
	a.kubeServiceName = *serviceName

	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// creates the clientset
	a.kubeClient, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// get my service
	a.myService, err = a.kubeClient.CoreV1().Services(a.kubeNamespace).Get(context.TODO(), a.kubeServiceName, metav1.GetOptions{})
	if err != nil {
		log.Fatalf("failed to get my service %s/%s: %s", a.kubeNamespace, a.kubeServiceName, err)
	}

	// register prometheus metrics
	prometheus.MustRegister(a.metricDownloadProbeSize)
	prometheus.MustRegister(a.metricDownloadDurations)
	prometheus.MustRegister(a.metricPingDurations)
	prometheus.MustRegister(a.metricHTTPDurations)
	prometheus.MustRegister(a.metricDNSLookupDurations)
	prometheus.MustRegister(a.metricTCPConnectionDurations)
	prometheus.MustRegister(a.metricStartTransferDurations)

	// start periodic test
	go a.testLoop()

	// start webserver
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/ping", a.handlePing)
	http.HandleFunc("/data", a.handleData)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

func (a *App) testDownload(source, dest *Labels) {
	url := fmt.Sprintf("%s://%s:%d/data", dest.PodPortName, dest.PodFQDN, dest.PodPort)
	result, end, err := a.testHTTP(url)
	if err != nil {
		log.Warnf("test download from '%s' failed: %s", url, err)
	}
	var labels []string
	labels = append(labels, source.Values()...)
	labels = append(labels, dest.Values()...)

	var elapsedTime = result.ContentTransfer(end).Seconds()
	log.Infof("test ping from '%s' took: [%fs]", url, elapsedTime)

	a.metricDownloadDurations.WithLabelValues(labels...).Observe(elapsedTime)
	a.metricDownloadProbeSize.WithLabelValues(labels...).Set(float64(*dataSize))
}

func (a *App) testPing(source, dest *Labels) {
	url := fmt.Sprintf("%s://%s:%d/ping", dest.PodPortName, dest.PodFQDN, dest.PodPort)
	var labels []string
	labels = append(labels, source.Values()...)
	labels = append(labels, dest.Values()...)
	for i := 1; i <= 5; i++ {
		result, end, err := a.testHTTP(url)
		if err != nil {
			log.Warnf("test ping from '%s' failed: %s", url, err)
		}
		var elapsedTime = result.ContentTransfer(end).Seconds()
		log.Debugf("test http transfer from '%s' took: [%fs]", url, elapsedTime)
		a.metricHTTPDurations.WithLabelValues(labels...).Observe(elapsedTime)

		var totalTime = result.Total(end).Seconds()
		log.Infof("test ping from '%s' took: [%fs]", url, totalTime)
		a.metricPingDurations.WithLabelValues(labels...).Observe(totalTime)

		var dnsLookupTime = result.DNSLookup.Seconds()
		log.Debugf("test dns lookup from '%s' took: [%fs]", url, dnsLookupTime)
		a.metricDNSLookupDurations.WithLabelValues(labels...).Observe(dnsLookupTime)

		var tcpTime = result.TCPConnection.Seconds()
		log.Debugf("test tcp connect from '%s' took: [%fs]", url, tcpTime)
		a.metricTCPConnectionDurations.WithLabelValues(labels...).Observe(tcpTime)

		var serverProcessing = result.ServerProcessing.Seconds()
		log.Debugf("test server processing time from '%s' took: [%fs]", url, serverProcessing)
		a.metricServerProcessingDurations.WithLabelValues(labels...).Observe(serverProcessing)

		var startTransfer = result.StartTransfer.Seconds()
		log.Debugf("test http connect time from '%s' took: [%fs]", url, startTransfer)
		a.metricStartTransferDurations.WithLabelValues(labels...).Observe(startTransfer)
	}
}

func (a *App) testHTTP(url string) (httpstat.Result, time.Time, error) {

	// Create a new HTTP request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Create a httpstat powered context
	var result httpstat.Result
	ctx := httpstat.WithHTTPStat(req.Context(), &result)
	req = req.WithContext(ctx)
	// Send request by default HTTP client
	client := http.DefaultClient
	res, err := client.Do(req)
	if err != nil {
		return result, time.Time{}, err
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		return result, time.Time{}, err
	}
	res.Body.Close()
	end := time.Now()
	return result, end, nil
}
