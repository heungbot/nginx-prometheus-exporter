package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	plusclient "github.com/nginx/nginx-plus-go-client/v2/client"
	"github.com/nginx/nginx-prometheus-exporter/client"
	"github.com/nginx/nginx-prometheus-exporter/collector"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/promslog/flag"
	common_version "github.com/prometheus/common/version"

	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"
)

// positiveDuration is a wrapper of time.Duration to ensure only positive values are accepted.
// Timeout Flag에 음수가 들어오는 것을 방지하기 위해 사용한다.
type positiveDuration struct{ time.Duration }

func (pd *positiveDuration) Set(s string) error {
	dur, err := parsePositiveDuration(s)
	if err != nil {
		return err
	}

	pd.Duration = dur.Duration
	return nil
}

func parsePositiveDuration(s string) (positiveDuration, error) {
	dur, err := time.ParseDuration(s)
	if err != nil {
		return positiveDuration{}, fmt.Errorf("failed to parse duration %q: %w", s, err)
	}
	if dur < 0 {
		return positiveDuration{}, fmt.Errorf("negative duration %v is not valid", dur)
	}
	return positiveDuration{dur}, nil
}

func createPositiveDurationFlag(s kingpin.Settings) (target *time.Duration) {
	target = new(time.Duration)
	s.SetValue(&positiveDuration{Duration: *target})
	return
}

func parseUnixSocketAddress(address string) (string, string, error) {
	addressParts := strings.Split(address, ":")
	addressPartsLength := len(addressParts)

	if addressPartsLength > 3 || addressPartsLength < 1 {
		return "", "", errors.New("address for unix domain socket has wrong format")
	}

	unixSocketPath := addressParts[1]
	requestPath := ""
	if addressPartsLength == 3 {
		requestPath = addressParts[2]
	}
	return unixSocketPath, requestPath, nil
}

var (
	constLabels = map[string]string{}

	// Command-line flags.
	webConfig     = kingpinflag.AddFlags(kingpin.CommandLine, ":9113")
	metricsPath   = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").Envar("TELEMETRY_PATH").String()
	nginxPlus     = kingpin.Flag("nginx.plus", "Start the exporter for NGINX Plus. By default, the exporter is started for NGINX.").Default("false").Envar("NGINX_PLUS").Bool()
	scrapeURIs    = kingpin.Flag("nginx.scrape-uri", "A URI or unix domain socket path for scraping NGINX or NGINX Plus metrics. For NGINX, the stub_status page must be available through the URI. For NGINX Plus -- the API. Repeatable for multiple URIs.").Default("http://127.0.0.1:8080/stub_status").Envar("SCRAPE_URI").HintOptions("http://127.0.0.1:8080/stub_status", "http://127.0.0.1:8080/api").Strings()
	sslVerify     = kingpin.Flag("nginx.ssl-verify", "Perform SSL certificate verification.").Default("false").Envar("SSL_VERIFY").Bool()
	sslCaCert     = kingpin.Flag("nginx.ssl-ca-cert", "Path to the PEM encoded CA certificate file used to validate the servers SSL certificate.").Default("").Envar("SSL_CA_CERT").String()
	sslClientCert = kingpin.Flag("nginx.ssl-client-cert", "Path to the PEM encoded client certificate file to use when connecting to the server.").Default("").Envar("SSL_CLIENT_CERT").String()
	sslClientKey  = kingpin.Flag("nginx.ssl-client-key", "Path to the PEM encoded client certificate key file to use when connecting to the server.").Default("").Envar("SSL_CLIENT_KEY").String()

	// Custom command-line flags.
	timeout         = createPositiveDurationFlag(kingpin.Flag("nginx.timeout", "A timeout for scraping metrics from NGINX or NGINX Plus.").Default("5s").Envar("TIMEOUT").HintOptions("5s", "10s", "30s", "1m", "5m"))
	nginxConfigPath = kingpin.Flag("nginx.config-path", "Path to the NGINX configuration file.").Default("/etc/nginx/nginx.conf").Envar("CONFIG_PATH").String()
)

const exporterName = "nginx_exporter"

func main() {
	kingpin.Flag("prometheus.const-label", "Label that will be used in every metric. Format is label=value. It can be repeated multiple times.").Envar("CONST_LABELS").StringMapVar(&constLabels)

	// convert deprecated flags to new format
	for i, arg := range os.Args {
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && len(arg) > 2 {
			newArg := "-" + arg
			fmt.Printf("the flag format is deprecated and will be removed in a future release, please use the new format: %s\n", newArg)
			os.Args[i] = newArg
		}
	}

	config := &promslog.Config{} // Log 설정을 위한 구조체 생성

	flag.AddFlags(kingpin.CommandLine, config) // log관련 flag 추가
	kingpin.Version(common_version.Print(exporterName))
	kingpin.HelpFlag.Short('h')

	addMissingEnvironmentFlags(kingpin.CommandLine)

	kingpin.Parse()
	logger := promslog.New(config)

	logger.Info("nginx-prometheus-exporter", "version", common_version.Info())
	logger.Info("build context", "build_context", common_version.BuildContext())

	// exporter의 이름 및 버전 등의 정보를 /metrics 경로에 함께 노출하도록 등록
	prometheus.MustRegister(version.NewCollector(exporterName))

	if len(*scrapeURIs) == 0 {
		logger.Error("no scrape addresses provided")
		os.Exit(1)
	}

	// #nosec G402
	sslConfig := &tls.Config{InsecureSkipVerify: !*sslVerify}
	if *sslCaCert != "" {
		caCert, err := os.ReadFile(*sslCaCert)
		if err != nil {
			logger.Error("loading CA cert failed", "err", err.Error())
			os.Exit(1)
		}
		sslCaCertPool := x509.NewCertPool()
		ok := sslCaCertPool.AppendCertsFromPEM(caCert)
		if !ok {
			logger.Error("parsing CA cert file failed.")
			os.Exit(1)
		}
		sslConfig.RootCAs = sslCaCertPool
	}

	if *sslClientCert != "" && *sslClientKey != "" {
		clientCert, err := tls.LoadX509KeyPair(*sslClientCert, *sslClientKey)
		if err != nil {
			logger.Error("loading client certificate failed", "error", err.Error())
			os.Exit(1)
		}
		sslConfig.Certificates = []tls.Certificate{clientCert}
	}

	transport := &http.Transport{
		TLSClientConfig: sslConfig,
	}

	// scrapeURIs는 여러 개일 수 있으므로, 각각에 대해 collector를 등록한다.
	// 여러 개일 경우, constLabels에 addr라는 레이블을 추가하여 구분할 수 있도록 한다.
	if len(*scrapeURIs) == 1 {
		registerCollector(logger, transport, (*scrapeURIs)[0], constLabels)
	} else {
		for _, addr := range *scrapeURIs {
			// add scrape URI to const labels
			labels := maps.Clone(constLabels)
			labels["addr"] = addr

			registerCollector(logger, transport, addr, labels)
		}
	}

	http.Handle(*metricsPath, promhttp.Handler())

	if *metricsPath != "/" && *metricsPath != "" {
		landingConfig := web.LandingConfig{
			Name:        "NGINX Prometheus Exporter",
			Description: "Prometheus Exporter for NGINX and NGINX Plus",
			HeaderColor: "#039900",
			Version:     common_version.Info(),
			Links: []web.LandingLinks{
				{
					Address: *metricsPath,
					Text:    "Metrics",
				},
			},
		}
		landingPage, err := web.NewLandingPage(landingConfig)
		if err != nil {
			logger.Error("failed to create landing page", "error", err.Error())
			os.Exit(1)
		}
		http.Handle("/", landingPage)
	}

	// graceful shutdown을 위해 signal.NotifyContext를 사용한다.
	// 인자로 받은 os.Interrupt, os.Kill, syscall.SIGTERM 시그널을 감지 시, 자동으로 취소되는 context이다.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill, syscall.SIGTERM)
	defer cancel()

	srv := &http.Server{ // HTTP 서버 인스턴스 생성
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 별도의 goroutine에서 HTTP 서버를 시작.
	// 이후 <-ctx.Done()이 올 때 까지 대기.
	go func() {
		if err := web.ListenAndServe(srv, webConfig, logger); err != nil {
			if errors.Is(err, http.ErrServerClosed) {
				logger.Info("HTTP server closed", "error", err.Error())
				os.Exit(0)
			}
			logger.Error("HTTP server failed", "error", err.Error())
			os.Exit(1)
		}
	}()

	<-ctx.Done() // Context에 Done 시그널을 보내 goroutine을 종료하고, 대기 중이던 메인 goroutine이 진행된다.
	logger.Info("shutting down")

	// 서버가 종료 신호를 받았을 때 클라 요청을 안전하게 마무리하고 종료하기 위해, 서버 종료 작업에 최대 5초의 제한 시간을 둔 컨텍스트를 생성.
	srvCtx, srvCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer srvCancel()
	_ = srv.Shutdown(srvCtx)
}

func registerCollector(logger *slog.Logger, transport *http.Transport,
	addr string, labels map[string]string,
) {
	if strings.HasPrefix(addr, "unix:") {
		socketPath, requestPath, err := parseUnixSocketAddress(addr)
		if err != nil {
			logger.Error("parsing unix domain socket scrape address failed", "uri", addr, "error", err.Error())
			os.Exit(1)
		}

		// scrape-uri가 unix 경로로 시작하는 경우, transport.DialContext를 재설정한다.
		// 즉, 표준 TCP 연결 대신, 유닉스 도메인 소켓을 사용하도록 지시한다.
		transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", socketPath)
		}
		addr = "http://unix" + requestPath
	}

	userAgent := fmt.Sprintf("NGINX-Prometheus-Exporter/v%v", common_version.Version)

	// HTTP 클라를 생성하는데, 다른 점이 있다면, userAgentRoundTripper를 사용한다는 것이다.
	// userAgentRoundTripper는 HTTP 요청에 User-Agent 헤더를 추가하는 역할을 한다.
	httpClient := &http.Client{
		Timeout: *timeout,
		Transport: &userAgentRoundTripper{
			agent: userAgent,
			rt:    transport,
		},
	}

	if *nginxPlus {
		plusClient, err := plusclient.NewNginxClient(addr, plusclient.WithHTTPClient(httpClient))
		if err != nil {
			logger.Error("could not create Nginx Plus Client", "error", err.Error())
			os.Exit(1)
		}
		variableLabelNames := collector.NewVariableLabelNames(nil, nil, nil, nil, nil, nil, nil)
		prometheus.MustRegister(collector.NewNginxPlusCollector(plusClient, "nginxplus", variableLabelNames, labels, logger))

	} else {
		// 여기서 Nginx Client를 사용하여 stub_status를 수집한다.
		ossClient := client.NewNginxClient(httpClient, addr)
		prometheus.MustRegister(collector.NewNginxCollector(ossClient, "nginx", labels, logger, *nginxConfigPath))
	}
}

// RTT(Round Trip Time) : 패킷이 클라이언트와 서버 사이를 왕복하는데 걸리는 시간
// 즉, RoundTrip은 HTTP 요청을 보내고 응답을 받는 과정을 의미한다.
// userAgentRoundTripper 기존 http.RoundTripper를 감싸서, 요청을 보내기 전에 User-Agent 헤더를 추가한다.
// 더불어 구현한 method는 모두 RoundTripper Interface에 속하기 위한 메서드이다. 즉, 코드에서 메서드를 직접 호출하지 않아도 사용되는 것이다.

type userAgentRoundTripper struct {
	rt    http.RoundTripper
	agent string
}

func (rt *userAgentRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = cloneRequest(req)
	req.Header.Set("User-Agent", rt.agent)
	roundTrip, err := rt.rt.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("round trip failed: %w", err)
	}
	return roundTrip, nil
}

func cloneRequest(req *http.Request) *http.Request {
	r := new(http.Request)
	*r = *req // 얕은 복사

	// 깊은 복사
	r.Header = make(http.Header, len(req.Header))
	for key, values := range req.Header {
		newValues := make([]string, len(values))
		copy(newValues, values)
		r.Header[key] = newValues
	}
	return r
}

// addMissingEnvironmentFlags sets Envar on any flag which has
// the "web." prefix which doesn't already have an Envar set.
func addMissingEnvironmentFlags(ka *kingpin.Application) {
	for _, f := range ka.Model().Flags {
		if strings.HasPrefix(f.Name, "web.") && f.Envar == "" {
			retrievedFlag := ka.GetFlag(f.Name)
			if retrievedFlag != nil {
				retrievedFlag.Envar(convertFlagToEnvar(strings.TrimPrefix(f.Name, "web.")))
			}
		}
	}
}

func convertFlagToEnvar(f string) string {
	env := strings.ToUpper(f)
	for _, s := range []string{"-", "."} {
		env = strings.ReplaceAll(env, s, "_")
	}
	return env
}
