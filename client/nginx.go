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
	"sync"
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
	Target             string // Proxy Target 주소(IP)
	TargetUpstreamName string // Proxy Target의 Upstream 이름
	Health             HealthCheckResult
}

// CustomStats 모든 설정 파일의 통계를 파일 경로를 키로 하여 맵으로 저장하는 구조체.
type CustomStats struct {
	ModifiedTimes   map[string]time.Time
	UpstreamHealths map[string][]UpstreamTargetHealth
}

// HealthCheckResult : 헬스체크 결과를 나타내는 타입 1.0은 성공을 의미하며, 0.0은 실패를 의미한다.
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
		if err == nil && !dir.IsDir() && filepath.Ext(path) == ".conf" {
			files = append(files, path)
		}
		return nil
	})

	var (
		wg      sync.WaitGroup
		statsMu sync.Mutex
	)

	// 파일 목록에 대한 병렬 처리를 위해 Goroutine 생성.
	for _, file := range files {
		wg.Add(1)

		go func(file string) {
			defer wg.Done()

			info, err := os.Stat(file)
			if err != nil {
				return
			}

			fileContent, err := os.ReadFile(file)
			if err != nil {
				return
			}

			upstreamTargets, _ := findAllUpstreamServers(string(fileContent))
			noneUpstreamTargets, _ := findProxyPassNotUpstream(string(fileContent))
			if len(noneUpstreamTargets) > 0 {
				upstreamTargets["none_upstream"] = noneUpstreamTargets
			}

			// Upstream Targets가 없는 경우, 파일의 수정 시각만 업데이트하고 종료
			// 공유 변수에 접근하기 때문에 Mutex 사용.
			if len(upstreamTargets) == 0 {
				statsMu.Lock()
				customStats.ModifiedTimes[file] = info.ModTime()
				statsMu.Unlock()
				return
			}

			var (
				innerWg sync.WaitGroup
				healths []UpstreamTargetHealth
				innerMu sync.Mutex
			)

			// 각 Upstream Target에 대해 TCP 연결 상태를 비동기적으로 확인을 위한 Goroutine 추가 생성
			for upstreamName, targets := range upstreamTargets {
				for _, target := range targets {
					innerWg.Add(1)
					go func(upstreamName, target string) {
						defer innerWg.Done()
						result, _ := tcpTest(target)
						innerMu.Lock() // 마찬가지로 Mutex로 공유 변수 접근(Race Condition 방지)
						healths = append(healths, UpstreamTargetHealth{
							Target:             target,
							TargetUpstreamName: upstreamName,
							Health:             result,
						})
						innerMu.Unlock()
					}(upstreamName, target)
				}
			}
			innerWg.Wait()

			statsMu.Lock()
			customStats.ModifiedTimes[file] = info.ModTime()
			customStats.UpstreamHealths[file] = healths
			statsMu.Unlock()
		}(file)
	}

	wg.Wait()
	return customStats, nil
}
