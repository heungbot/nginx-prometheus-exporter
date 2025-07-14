package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const templateMetrics string = `Active connections: %d
server accepts handled requests
%d %d %d
Reading: %d Writing: %d Waiting: %d
`

// NginxClient allows you to fetch NGINX metrics from the stub_status page.
type NginxClient struct {
	httpClient  *http.Client
	apiEndpoint string
}

// StubStats represents NGINX stub_status metrics.
type StubStats struct {
	Connections StubConnections
	Requests    int64
}

// StubConnections represents connections related metrics.
type StubConnections struct {
	Active   int64
	Accepted int64
	Handled  int64
	Reading  int64
	Writing  int64
	Waiting  int64
}

// UpstreamTargetHealth 개별 프록시 타겟의 헬스체크 상태를 저장하는 구조체.
type UpstreamTargetHealth struct {
	Target string
	Health HealthCheckResult
}

// CustomStats 모든 설정 파일의 통계를 파일 경로를 키로 하여 맵으로 저장하는 구조체.
type CustomStats struct {
	ModifiedTimes   map[string]time.Time
	UpstreamHealths map[string][]UpstreamTargetHealth
}

type HealthCheckResult float32

const (
	TcpSuccess = 1.0
	TcpFailure = 0.0
)

// NewNginxClient creates an NginxClient.
func NewNginxClient(httpClient *http.Client, apiEndpoint string) *NginxClient {
	client := &NginxClient{
		apiEndpoint: apiEndpoint,
		httpClient:  httpClient,
	}

	return client
}

// GetStubStats fetches the stub_status metrics.
func (client *NginxClient) GetStubStats() (*StubStats, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.apiEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create a get request: %w", err)
	}
	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get %v: %w", client.apiEndpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("expected %v response, got %v", http.StatusOK, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read the response body: %w", err)
	}

	r := bytes.NewReader(body)
	stats, err := parseStubStats(r)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response body %q: %w", string(body), err)
	}

	return stats, nil
}

func parseStubStats(r io.Reader) (*StubStats, error) {
	var s StubStats
	if _, err := fmt.Fscanf(r, templateMetrics,
		&s.Connections.Active,
		&s.Connections.Accepted,
		&s.Connections.Handled,
		&s.Requests,
		&s.Connections.Reading,
		&s.Connections.Writing,
		&s.Connections.Waiting); err != nil {
		return nil, fmt.Errorf("failed to scan template metrics: %w", err)
	}
	return &s, nil
}

// GetCustomStats : Proxy 서버 모니터링을 위한 커스텀 메트릭을 반환하는 메서드.
// 이 메서드는 NGINX 설정 파일의 마지막 수정 시각과 Proxy Target의 TCP 연결 상태를 포함한다.
func (client *NginxClient) GetCustomStats(nginxConfigPath string) (*CustomStats, error) {
	customStats := &CustomStats{
		ModifiedTimes:   make(map[string]time.Time),
		UpstreamHealths: make(map[string][]UpstreamTargetHealth),
	}

	files := []string{nginxConfigPath}
	confDir := filepath.Join(filepath.Dir(nginxConfigPath), "conf.d")
	_ = filepath.WalkDir(confDir, func(path string, dir fs.DirEntry, err error) error {
		if err == nil && !dir.IsDir() {
			files = append(files, path)
		}
		return nil
	})

	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			continue
		}

		customStats.ModifiedTimes[file] = info.ModTime()

		proxyTargetServers, _ := extractProxyTarget(file)

		var healths []UpstreamTargetHealth
		for _, target := range proxyTargetServers {
			result, _ := tcpTest(target)
			healths = append(healths, UpstreamTargetHealth{
				Target: target,
				Health: result,
			})
		}
		customStats.UpstreamHealths[file] = healths
	}
	return customStats, nil
}
