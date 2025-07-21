package client

import (
	"net"
	"regexp"
	"strings"
	"time"
)

// findAllUpstreamServers : nginx 설정 파일 내용에서 모든 upstream 블록과 서버 목록을 찾아 map으로 반환하는 함수.
func findAllUpstreamServers(content string) (map[string][]string, error) {
	upstreams := make(map[string][]string)

	reUpstreamBlock := regexp.MustCompile(`upstream\s+([^\s{]+)\s*\{([\s\S]*?)\}`)
	allUpstreamMatches := reUpstreamBlock.FindAllStringSubmatch(content, -1)

	reServer := regexp.MustCompile(`server\s+([^; ]+);`)

	for _, upstreamMatch := range allUpstreamMatches {
		if len(upstreamMatch) < 3 {
			continue
		}
		upstreamName := upstreamMatch[1]
		upstreamContent := upstreamMatch[2]

		var servers []string
		serverMatches := reServer.FindAllStringSubmatch(upstreamContent, -1)
		for _, serverMatch := range serverMatches {
			if len(serverMatch) > 1 {
				servers = append(servers, serverMatch[1])
			}
		}

		if len(servers) > 0 {
			upstreams[upstreamName] = servers
		}
	}

	return upstreams, nil
}

// tcpTest : proxyTarget 인자를 받아 TCP 연결을 테스트하는 함수.
func tcpTest(proxyTarget string) (result HealthCheckResult, err error) {
	if !strings.Contains(proxyTarget, ":") {
		proxyTarget = proxyTarget + ":80"
	}

	conn, err := net.DialTimeout("tcp", proxyTarget, 5*time.Second)
	if err != nil {
		return TcpFailure, nil
	} else if conn != nil {
		_ = conn.Close()
		return TcpSuccess, nil
	} else {
		return TcpFailure, nil
	}
}
