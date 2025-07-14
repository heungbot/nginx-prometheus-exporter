package collector

import (
	"log/slog"
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
		nginxClient:     nginxClient,
		nginxConfigPath: nginxConfigPath, // Custom을 위한 추가.
		logger:          logger,
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

		// Custom Metric 추가 부분 //
		configModDesc: prometheus.NewDesc(
			// nginx_config_last_modified_seconds
			prometheus.BuildFQName(namespace, "config", "last_modified_seconds"),
			"NGINX config 파일별 마지막 수정 시각(Unix timestamp)",
			[]string{"file"}, constLabels,
		),

		upstreamHealthCheckDesc: prometheus.NewDesc(
			// nginx_upstream_health_check_status
			prometheus.BuildFQName(namespace, "upstream", "health_check_status"),
			"Proxy Target의 TCP 연결 상태(1: 성공, 0: 실패)",
			[]string{"file", "target"}, constLabels,
		),
	}
}

// Describe sends the super-set of all possible descriptors of NGINX metrics
// to the provided channel.
func (c *NginxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.upMetric.Desc()
	ch <- c.configModDesc
	ch <- c.upstreamHealthCheckDesc

	for _, m := range c.metrics {
		ch <- m
	}
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

	// 커스텀 메트릭 추가 부분 //
	customStats, err := c.nginxClient.GetCustomStats(c.nginxConfigPath)
	if err != nil {
		c.logger.Warn("error getting custom stats", "error", err)
		return
	}
	for file, modTime := range customStats.ModifiedTimes {
		ch <- prometheus.MustNewConstMetric(
			c.configModDesc,
			prometheus.GaugeValue,
			float64(modTime.Unix()),
			file,
		)
	}

	for file, healths := range customStats.UpstreamHealths {
		for _, health := range healths {
			ch <- prometheus.MustNewConstMetric(
				c.upstreamHealthCheckDesc,
				prometheus.GaugeValue,
				float64(health.Health),
				file,
				health.Target,
			)
		}
	}
}
