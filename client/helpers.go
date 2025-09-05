package client

import (
	"net"
	"regexp"
	"strings"
	"time"
)

// findAllUpstreamServers : NGINX 설정 파일에서 모든 Upstream Block을 찾아서 map 형태로 반환하는 함수
func findAllUpstreamServers(content string) (map[string][]string, error) {
	upstreams := make(map[string][]string)
	reUpstreamBlock := regexp.MustCompile(`upstream\s+([^\s{]+)\s*\{([\s\S]*?)\}`)
	allUpstreamMatches := reUpstreamBlock.FindAllStringSubmatch(content, -1)
	reServer := regexp.MustCompile(`(?m)^\s*server\s+([^; ]+);`)

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

// findProxyPassNotUpstream : NGINX 설정 파일에서 Upstream Block에 정의되지 않은 proxy_pass 대상들을 찾아서 반환하는 함수
func findProxyPassNotUpstream(content string) ([]string, error) {
	//re := regexp.MustCompile(`proxy_pass\s+(?:http://)?([^; ]*\.[^; ]+);`)
	re := regexp.MustCompile(`(?m)^\s*proxy_pass\s+(?:http://)?([^; ]*\.[^; ]+);`)
	matches := re.FindAllStringSubmatch(content, -1)

	var targets []string
	for _, match := range matches {
		if len(match) > 1 {
			target := match[1]
			// target이 IP 주소 또는 도메인 이름 형식인지 확인
			// upstream 이름과 같은 경우는 보통 '.'이나 ':'를 포함하지 않음
			if strings.Contains(target, ".") || strings.Contains(target, ":") {
				targets = append(targets, target)
			}
		}
	}

	return targets, nil
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
