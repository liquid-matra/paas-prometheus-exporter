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
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/cloudfoundry-community/go-cfclient/v2"
	cfenv "github.com/cloudfoundry-community/go-cfenv"
	"github.com/prometheus/client_golang/prometheus"
)

var (
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
	flag.StringVar(&apiEndpoint, "api-endpoint", LookupEnvOrString("API_ENDPOINT", ""), "API endpoint")
	flag.StringVar(&logCacheEndpoint, "logcache-endpoint", LookupEnvOrString("API_ENDPOINT", ""), "LogCache endpoint")
	flag.StringVar(&username, "username", LookupEnvOrString("USERNAME", ""), "Cloud Foundry username")
	flag.StringVar(&username, "password", LookupEnvOrString("PASSWORD", ""), "Cloud Foundry password")
	flag.StringVar(&username, "client-id", LookupEnvOrString("CLIENT_ID", ""), "uaa Client ID")
	flag.StringVar(&username, "client-secret", LookupEnvOrString("CLIENT_SECRET", ""), "uaa Client ID")
	flag.Int64Var(&updateFrequency, "update-frequency", 300, "The time in seconds, that takes between each apps update call")
	flag.Int64Var(&scrapeInterval, "scrape-interval", 60, "The time in seconds, that takes between Prometheus scrapes")
	flag.IntVar(&prometheusBindPort, "prometheus-bind-port", 60, "tThe port to bind to for prometheus metrics")
	flag.StringVar(&username, "auth-username", LookupEnvOrString("AUTH_USERNAME", ""), "HTTP basic auth username; leave blank to disable basic auth")
	flag.StringVar(&username, "auth-password", LookupEnvOrString("AUTH_PASSWORD", ""), "HTTP basic auth password")
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
	CheckConfig()

	buildInfo := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "paas_exporter_build_info",
			Help: "PaaS Prometheus exporter build info.",
			ConstLabels: prometheus.Labels{
				"version": version,
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
		version,
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

func CheckConfig() {
	var missingVariable []string
	if username == "" {
		missingVariable = append(missingVariable, "USERNAME")
	}
	if password == "" {
		missingVariable = append(missingVariable, "PASSWORD")
	}
	if clientID == "" {
		missingVariable = append(missingVariable, "CLIENT_ID")
	}
	if clientSecret == "" {
		missingVariable = append(missingVariable, "CLIENT_SECRET")
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
}
