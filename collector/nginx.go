package collector

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nginx/nginx-prometheus-exporter/client"
	"github.com/prometheus/client_golang/prometheus"
)

// NginxCollector collects NGINX metrics. It implements prometheus.Collector interface.
type NginxCollector struct {
	upMetric    prometheus.Gauge
	logger      *slog.Logger
	nginxClient *client.NginxClient
	metrics     map[string]*prometheus.Desc
	mutex       sync.Mutex

	// Custom For Nginx Proxy //
	nginxConfigPath         string
	configModDesc           *prometheus.Desc
	upstreamHealthCheckDesc *prometheus.Desc
}

// NewNginxCollector creates an NginxCollector.
func NewNginxCollector(nginxClient *client.NginxClient, namespace string, constLabels map[string]string, logger *slog.Logger, nginxConfigPath string) *NginxCollector {
	return &NginxCollector{
		nginxClient: nginxClient,
		logger:      logger,
		metrics: map[string]*prometheus.Desc{
			"connections_active":   newGlobalMetric(namespace, "connections_active", "Active client connections", constLabels),
			"connections_accepted": newGlobalMetric(namespace, "connections_accepted", "Accepted client connections", constLabels),
			"connections_handled":  newGlobalMetric(namespace, "connections_handled", "Handled client connections", constLabels),
			"connections_reading":  newGlobalMetric(namespace, "connections_reading", "Connections where NGINX is reading the request header", constLabels),
			"connections_writing":  newGlobalMetric(namespace, "connections_writing", "Connections where NGINX is writing the response back to the client", constLabels),
			"connections_waiting":  newGlobalMetric(namespace, "connections_waiting", "Idle client connections", constLabels),
			"http_requests_total":  newGlobalMetric(namespace, "http_requests_total", "Total http requests", constLabels),
		},
		upMetric: newUpMetric(namespace, constLabels),
		configModDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "config", "last_modified_seconds"),
			"NGINX config 파일별 마지막 수정 시각(Unix timestamp)",
			[]string{"file"}, constLabels,
		),
		upstreamHealthCheckDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "upstream", "health_check_status"),
			"Proxy Target의 TCP 연결 상태(1: 성공, 0: 실패)",
			[]string{"file", "target"}, constLabels,
		),
		nginxConfigPath: nginxConfigPath,
	}
}

// Describe sends the super-set of all possible descriptors of NGINX metrics
// to the provided channel.
func (c *NginxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.upMetric.Desc()

	for _, m := range c.metrics {
		ch <- m
	}

	ch <- c.configModDesc
	ch <- c.upstreamHealthCheckDesc
}

// Collect fetches metrics from NGINX and sends them to the provided channel.
func (c *NginxCollector) Collect(ch chan<- prometheus.Metric) {
	c.mutex.Lock() // To protect metrics from concurrent collects
	defer c.mutex.Unlock()

	stats, err := c.nginxClient.GetStubStats()
	if err != nil {
		c.upMetric.Set(nginxDown)
		ch <- c.upMetric
		c.logger.Error("error getting stats", "error", err.Error())
		return
	}

	c.upMetric.Set(nginxUp)
	ch <- c.upMetric

	ch <- prometheus.MustNewConstMetric(c.metrics["connections_active"],
		prometheus.GaugeValue, float64(stats.Connections.Active))
	ch <- prometheus.MustNewConstMetric(c.metrics["connections_accepted"],
		prometheus.CounterValue, float64(stats.Connections.Accepted))
	ch <- prometheus.MustNewConstMetric(c.metrics["connections_handled"],
		prometheus.CounterValue, float64(stats.Connections.Handled))
	ch <- prometheus.MustNewConstMetric(c.metrics["connections_reading"],
		prometheus.GaugeValue, float64(stats.Connections.Reading))
	ch <- prometheus.MustNewConstMetric(c.metrics["connections_writing"],
		prometheus.GaugeValue, float64(stats.Connections.Writing))
	ch <- prometheus.MustNewConstMetric(c.metrics["connections_waiting"],
		prometheus.GaugeValue, float64(stats.Connections.Waiting))
	ch <- prometheus.MustNewConstMetric(c.metrics["http_requests_total"],
		prometheus.CounterValue, float64(stats.Requests))

	////// CUSTOM FOR NGINX PROXY //////
	files := []string{c.nginxConfigPath}                                 // []string{"/home1/irteam/apps/nginx/nginx.conf"}
	confdDir := filepath.Join(filepath.Dir(c.nginxConfigPath), "conf.d") // "/home1/irteam/apps/nginx/conf.d"
	// 순회 하면서 files slice에 추가.
	_ = filepath.WalkDir(confdDir, func(path string, dir fs.DirEntry, err error) error {
		if err == nil && !dir.IsDir() {
			files = append(files, path)
		}
		return nil
	})

	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil || !strings.HasSuffix(info.Name(), ".conf") {
			c.logger.Warn("skip config file", "file", f, "err", err)
			continue
		}

		proxyTargets, err := extractProxyTarget(f)
		if err != nil {
			c.logger.Warn("error extracting proxy targets", "file", f, "error", err.Error())
			continue
		}

		// prox target 추출 후, tcp 연결 테스트 수행
		for _, target := range proxyTargets {
			netResult, err := tcpTest(target)
			if err != nil {
				c.logger.Warn("error testing proxy target", "file", f, "target", target, "error", err.Error())
			}
			ch <- prometheus.MustNewConstMetric(
				c.upstreamHealthCheckDesc,
				prometheus.GaugeValue,
				netResult,
				f, target,
			)
		}

		// 파일의 마지막 수정 시각을 Unix timestamp로 치환하여 메트릭으로 전송
		ch <- prometheus.MustNewConstMetric(
			c.configModDesc,
			prometheus.GaugeValue,
			float64(info.ModTime().Unix()),
			f,
		)
	}
}
