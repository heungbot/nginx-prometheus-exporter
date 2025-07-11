package collector

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"net"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	nginxUp   = 1
	nginxDown = 0
)

func newGlobalMetric(namespace string, metricName string, docString string, constLabels map[string]string) *prometheus.Desc {
	return prometheus.NewDesc(namespace+"_"+metricName, docString, nil, constLabels)
}

func newUpMetric(namespace string, constLabels map[string]string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "up",
		Help:        "Status of the last metric scrape",
		ConstLabels: constLabels,
	})
}

// MergeLabels merges two maps of labels.
func MergeLabels(a map[string]string, b map[string]string) map[string]string {
	c := make(map[string]string)

	for k, v := range a {
		c[k] = v
	}
	for k, v := range b {
		c[k] = v
	}

	return c
}

// getProxyPassTarget : nginx.conf를 읽어 proxy_pass target을 가져오는 함수.
func extractProxyTarget(filePath string) ([]string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	contentStr := string(content)

	re := regexp.MustCompile(`proxy_pass\s+(.*?);`)
	matches := re.FindAllStringSubmatch(contentStr, -1)

	var targets []string
	for _, match := range matches {
		if len(match) > 1 {
			// match[1]은 proxy_pass 뒤의 URL 또는 upstream 이름. 해당 이름에 대해 전처리 수행.
			target := strings.TrimSpace(match[1])
			target = strings.TrimPrefix(target, "http://")
			target = strings.TrimPrefix(target, "https://")

			// 전처리된 이름이 IP or 도메인 형식이 아닐 아닐 경우, upstream 으로 간주.
			ipFormat := regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?$`)
			domainFormat := regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*(:\d+)?$`)

			if !ipFormat.MatchString(target) && !domainFormat.MatchString(target) {
				upstreamServers, err := findUpstreamServers(contentStr, target)
				if err == nil {
					targets = append(targets, upstreamServers...)
				}
			} else {
				targets = append(targets, target)
			}
		}
	}

	return targets, nil
}

// findUpstreamServers : upstream 블록에서 서버 주소를 찾습니다.
func findUpstreamServers(content, upstreamName string) ([]string, error) {
	// upstream 블록을 찾는 정규식
	reUpstreamBlock := regexp.MustCompile(fmt.Sprintf(`upstream\s+%s\s*\{([\s\S]*?)\}`, regexp.QuoteMeta(upstreamName)))
	blockMatch := reUpstreamBlock.FindStringSubmatch(content)
	if len(blockMatch) < 2 {
		return nil, fmt.Errorf("upstream block '%s' not found", upstreamName)
	}
	upstreamContent := blockMatch[1]

	// upstream 블록 내에서 server 주소를 찾는 정규식
	reServer := regexp.MustCompile(`server\s+([^; ]+);`)
	serverMatches := reServer.FindAllStringSubmatch(upstreamContent, -1)

	var servers []string
	for _, serverMatch := range serverMatches {
		if len(serverMatch) > 1 {
			servers = append(servers, serverMatch[1])
		}
	}

	return servers, nil
}

// tcpTest : proxyTarget 인자를 받아 TCP 연결을 테스트하는 함수.
func tcpTest(proxyTarget string) (result float64, err error) {
	if !strings.Contains(proxyTarget, ":") {
		proxyTarget = proxyTarget + ":80"
	}

	conn, err := net.DialTimeout("tcp", proxyTarget, 3*time.Second)
	if err != nil {
		return 0.0, nil
	} else if conn != nil {
		_ = conn.Close()
		return 1.0, nil
	} else {
		return 0.0, nil
	}
}
