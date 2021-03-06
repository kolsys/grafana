package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	gocache "github.com/patrickmn/go-cache"
	macaron "gopkg.in/macaron.v1"

	"github.com/p0hil/grafana/pkg/api/live"
	httpstatic "github.com/p0hil/grafana/pkg/api/static"
	"github.com/p0hil/grafana/pkg/bus"
	"github.com/p0hil/grafana/pkg/components/simplejson"
	"github.com/p0hil/grafana/pkg/log"
	"github.com/p0hil/grafana/pkg/middleware"
	"github.com/p0hil/grafana/pkg/models"
	"github.com/p0hil/grafana/pkg/plugins"
	"github.com/p0hil/grafana/pkg/setting"
)

type HTTPServer struct {
	log           log.Logger
	macaron       *macaron.Macaron
	context       context.Context
	streamManager *live.StreamManager
	cache         *gocache.Cache

	httpSrv *http.Server
}

func NewHTTPServer() *HTTPServer {
	return &HTTPServer{
		log:   log.New("http.server"),
		cache: gocache.New(5*time.Minute, 10*time.Minute),
	}
}

func (hs *HTTPServer) Start(ctx context.Context) error {
	var err error

	hs.context = ctx
	hs.streamManager = live.NewStreamManager()
	hs.macaron = hs.newMacaron()
	hs.registerRoutes()

	hs.streamManager.Run(ctx)

	listenAddr := fmt.Sprintf("%s:%s", setting.HttpAddr, setting.HttpPort)
	hs.log.Info("Initializing HTTP Server", "address", listenAddr, "protocol", setting.Protocol, "subUrl", setting.AppSubUrl, "socket", setting.SocketPath)

	hs.httpSrv = &http.Server{Addr: listenAddr, Handler: hs.macaron}
	switch setting.Protocol {
	case setting.HTTP:
		err = hs.httpSrv.ListenAndServe()
		if err == http.ErrServerClosed {
			hs.log.Debug("server was shutdown gracefully")
			return nil
		}
	case setting.HTTPS:
		err = hs.listenAndServeTLS(setting.CertFile, setting.KeyFile)
		if err == http.ErrServerClosed {
			hs.log.Debug("server was shutdown gracefully")
			return nil
		}
	case setting.SOCKET:
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: setting.SocketPath, Net: "unix"})
		if err != nil {
			hs.log.Debug("server was shutdown gracefully")
			return nil
		}

		// Make socket writable by group
		os.Chmod(setting.SocketPath, 0660)

		err = hs.httpSrv.Serve(ln)
		if err != nil {
			hs.log.Debug("server was shutdown gracefully")
			return nil
		}
	default:
		hs.log.Error("Invalid protocol", "protocol", setting.Protocol)
		err = errors.New("Invalid Protocol")
	}

	return err
}

func (hs *HTTPServer) Shutdown(ctx context.Context) error {
	err := hs.httpSrv.Shutdown(ctx)
	hs.log.Info("Stopped HTTP server")
	return err
}

func (hs *HTTPServer) listenAndServeTLS(certfile, keyfile string) error {
	if certfile == "" {
		return fmt.Errorf("cert_file cannot be empty when using HTTPS")
	}

	if keyfile == "" {
		return fmt.Errorf("cert_key cannot be empty when using HTTPS")
	}

	if _, err := os.Stat(setting.CertFile); os.IsNotExist(err) {
		return fmt.Errorf(`Cannot find SSL cert_file at %v`, setting.CertFile)
	}

	if _, err := os.Stat(setting.KeyFile); os.IsNotExist(err) {
		return fmt.Errorf(`Cannot find SSL key_file at %v`, setting.KeyFile)
	}

	tlsCfg := &tls.Config{
		MinVersion:               tls.VersionTLS12,
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		},
	}

	hs.httpSrv.TLSConfig = tlsCfg
	hs.httpSrv.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))

	return hs.httpSrv.ListenAndServeTLS(setting.CertFile, setting.KeyFile)
}

func (hs *HTTPServer) newMacaron() *macaron.Macaron {
	macaron.Env = setting.Env
	m := macaron.New()

	m.Use(middleware.Logger())

	if setting.EnableGzip {
		m.Use(middleware.Gziper())
	}

	m.Use(middleware.Recovery())

	for _, route := range plugins.StaticRoutes {
		pluginRoute := path.Join("/public/plugins/", route.PluginId)
		hs.log.Debug("Plugins: Adding route", "route", pluginRoute, "dir", route.Directory)
		hs.mapStatic(m, route.Directory, "", pluginRoute)
	}

	hs.mapStatic(m, setting.StaticRootPath, "", "public")
	hs.mapStatic(m, setting.StaticRootPath, "robots.txt", "robots.txt")

	if setting.ImageUploadProvider == "local" {
		hs.mapStatic(m, setting.ImagesDir, "", "/public/img/attachments")
	}

	m.Use(macaron.Renderer(macaron.RenderOptions{
		Directory:  path.Join(setting.StaticRootPath, "views"),
		IndentJSON: macaron.Env != macaron.PROD,
		Delims:     macaron.Delims{Left: "[[", Right: "]]"},
	}))

	m.Use(hs.healthHandler)
	m.Use(hs.metricsEndpoint)
	m.Use(middleware.GetContextHandler())
	m.Use(middleware.Sessioner(&setting.SessionOptions, setting.SessionConnMaxLifetime))
	m.Use(middleware.OrgRedirect())

	// needs to be after context handler
	if setting.EnforceDomain {
		m.Use(middleware.ValidateHostHeader(setting.Domain))
	}

	m.Use(middleware.AddDefaultResponseHeaders())

	return m
}

func (hs *HTTPServer) metricsEndpoint(ctx *macaron.Context) {
	if ctx.Req.Method != "GET" || ctx.Req.URL.Path != "/metrics" {
		return
	}

	promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}).
		ServeHTTP(ctx.Resp, ctx.Req.Request)
}

func (hs *HTTPServer) healthHandler(ctx *macaron.Context) {
	notHeadOrGet := ctx.Req.Method != http.MethodGet && ctx.Req.Method != http.MethodHead
	if notHeadOrGet || ctx.Req.URL.Path != "/api/health" {
		return
	}

	data := simplejson.New()
	data.Set("database", "ok")
	data.Set("version", setting.BuildVersion)
	data.Set("commit", setting.BuildCommit)

	if err := bus.Dispatch(&models.GetDBHealthQuery{}); err != nil {
		data.Set("database", "failing")
		ctx.Resp.Header().Set("Content-Type", "application/json; charset=UTF-8")
		ctx.Resp.WriteHeader(503)
	} else {
		ctx.Resp.Header().Set("Content-Type", "application/json; charset=UTF-8")
		ctx.Resp.WriteHeader(200)
	}

	dataBytes, _ := data.EncodePretty()
	ctx.Resp.Write(dataBytes)
}

func (hs *HTTPServer) mapStatic(m *macaron.Macaron, rootDir string, dir string, prefix string) {
	headers := func(c *macaron.Context) {
		c.Resp.Header().Set("Cache-Control", "public, max-age=3600")
	}

	if setting.Env == setting.DEV {
		headers = func(c *macaron.Context) {
			c.Resp.Header().Set("Cache-Control", "max-age=0, must-revalidate, no-cache")
		}
	}

	m.Use(httpstatic.Static(
		path.Join(rootDir, dir),
		httpstatic.StaticOptions{
			SkipLogging: true,
			Prefix:      prefix,
			AddHeaders:  headers,
		},
	))
}
