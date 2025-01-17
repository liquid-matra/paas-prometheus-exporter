package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/liquid-matra/paas-prometheus-exporter/app"
	"github.com/liquid-matra/paas-prometheus-exporter/cf"
	"github.com/liquid-matra/paas-prometheus-exporter/service"
	"github.com/liquid-matra/paas-prometheus-exporter/util"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cloudfoundry-community/go-cfclient/v2"
	cfenv "github.com/cloudfoundry-community/go-cfenv"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	Version            string
	apiEndpoint        string
	logCacheEndpoint   string
	username           string
	password           string
	clientID           string
	clientSecret       string
	updateFrequency    int64
	scrapeInterval     int64
	prometheusBindPort int
	authUsername       string
	authPassword       string
)

func loadEnvironmentVariables() {
	flag.BoolFunc("version", "prints version and quits", printVersion)
	flag.StringVar(&apiEndpoint, "api-endpoint", LookupEnvOrString("API_ENDPOINT", ""), "API endpoint")
	flag.StringVar(&logCacheEndpoint, "logcache-endpoint", LookupEnvOrString("API_ENDPOINT", ""), "LogCache endpoint")
	flag.StringVar(&username, "username", LookupEnvOrString("USERNAME", ""), "Cloud Foundry username")
	flag.StringVar(&password, "password", LookupEnvOrString("PASSWORD", ""), "Cloud Foundry password")
	flag.StringVar(&clientID, "client-id", LookupEnvOrString("CLIENT_ID", ""), "uaa Client ID")
	flag.StringVar(&clientSecret, "client-secret", LookupEnvOrString("CLIENT_SECRET", ""), "uaa Client Secret")
	flag.Int64Var(&updateFrequency, "update-frequency", strconv.Atoi(LookupEnvOrString("UPDATE_FREQUENCY", "300")), "The time in seconds, that takes between each apps update call")
	flag.Int64Var(&scrapeInterval, "scrape-interval", strconv.Atoi(LookupEnvOrString("SCRAPE_INTERVAL", "60")), "The time in seconds, that takes between Prometheus scrapes")
	flag.IntVar(&prometheusBindPort, "prometheus-bind-port", strconv.Atoi(LookupEnvOrString("PROMETHEUS_BIND_PORT", "60000")), "The port to bind to for prometheus metrics")
	flag.StringVar(&authUsername, "auth-username", LookupEnvOrString("AUTH_USERNAME", ""), "HTTP basic auth username; leave blank to disable basic auth")
	flag.StringVar(&authPassword, "auth-password", LookupEnvOrString("AUTH_PASSWORD", ""), "HTTP basic auth password")
}

func printVersion(s string) error {
	fmt.Println(Version)
	os.Exit(0)
	return nil
}

type ServiceDiscovery interface {
	Start()
	Stop()
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		cancel()
	}()

	loadEnvironmentVariables()
	flag.Parse()

	var missingVariable []string
	if username == "" {
		missingVariable = append(missingVariable, "USERNAME")
	}
	if password == "" {
		missingVariable = append(missingVariable, "PASSWORD")
	}
	if apiEndpoint == "" {
		missingVariable = append(missingVariable, "API_ENDPOINT")
	}

	if logCacheEndpoint == "" {
		logCacheEndpoint = strings.Replace(apiEndpoint, "api.", "log-cache.", 1)
	}

	if updateFrequency < 60 {
		log.Fatal("The update frequency can not be less than 1 minute")
		os.Exit(1)
	}

	if scrapeInterval < 60 {
		log.Fatal("The scrape interval can not be less than 1 minute")
		os.Exit(1)
	}

	if len(missingVariable) > 0 {
		log.Fatal("Missing mandatory environment variables:", missingVariable)
		os.Exit(1)
	}

	buildInfo := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "paas_exporter_build_info",
			Help: "PaaS Prometheus exporter build info.",
			ConstLabels: prometheus.Labels{
				"Version": Version,
			},
		},
	)
	buildInfo.Set(1)
	prometheus.DefaultRegisterer.MustRegister(buildInfo)

	vcapplication, err := cfenv.Current()
	if err != nil {
		log.Fatal("Could not decode the VCAP_APPLICATION environment variable")
	}

	appId := vcapplication.AppID
	appName := vcapplication.Name
	appIndex := vcapplication.Index

	// We set a unique user agent so we can
	// identify individual exporters in our logs
	userAgent := fmt.Sprintf(
		"paas-prometheus-exporter/%s (app=%s, index=%d, name=%s)",
		Version,
		appId,
		appIndex,
		appName,
	)

	config := &cfclient.Config{
		ApiAddress:   apiEndpoint,
		Username:     username,
		Password:     password,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		UserAgent:    userAgent,
	}
	client, err := cf.NewClient(config, logCacheEndpoint)
	if err != nil {
		log.Fatal(err)
	}

	errChan := make(chan error, 1)

	appDiscovery := app.NewDiscovery(
		client,
		prometheus.DefaultRegisterer,
		time.Duration(updateFrequency)*time.Second,
	)

	appDiscovery.Start(ctx, errChan)

	serviceDiscovery := service.NewDiscovery(
		client,
		prometheus.DefaultRegisterer,
		time.Duration(updateFrequency)*time.Second,
		time.Duration(scrapeInterval)*time.Second,
	)

	serviceDiscovery.Start(ctx, errChan)

	server := buildHTTPServer(prometheusBindPort, promhttp.Handler(), authUsername, authPassword)

	go func() {
		server.Addr = "0.0.0.0:8080"
		err := server.ListenAndServe()
		if err != nil {
			errChan <- err
		}
	}()

	for {
		select {

		case err := <-errChan:
			log.Println(err)
			cancel()

			defer func() {
				// This will appear as a CF app crash when the app encounters an error
				log.Println("cancel upon error finished. exiting with status code 1")
				os.Exit(1)
			}()

		case <-ctx.Done():
			log.Println("exiting")
			shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
			defer shutdownCancel()

			err := server.Shutdown(shutdownCtx)
			if err != nil {
				log.Println(err)
			}
			return
		}
	}
}

func buildHTTPServer(port int, promHandler http.Handler, authUsername, authPassword string) *http.Server {
	server := &http.Server{Addr: fmt.Sprintf(":%d", port)}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promHandler)
	server.Handler = mux

	if authUsername != "" {
		server.Handler = util.BasicAuthHandler(authUsername, authPassword, "metrics", server.Handler)
	}

	return server
}

func LookupEnvOrString(key string, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}
