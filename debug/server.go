// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

// Package debug implements debug server for satellite and storage node.
package debug

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/spacemonkeygo/monkit/v3/present"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"storj.io/private/traces"
	"storj.io/private/version"
)

func init() {
	// zero out the http.DefaultServeMux net/http/pprof so unhelpfully
	// side-effected.
	*http.DefaultServeMux = http.ServeMux{}
}

// Config defines configuration for debug server.
type Config struct {
	Address string `internal:"true"`

	ControlTitle string `internal:"true"`
	Control      bool   `help:"expose control panel" releaseDefault:"false" devDefault:"true"`
}

// Server provides endpoints for debugging.
type Server struct {
	log *zap.Logger

	listener net.Listener
	server   http.Server
	mux      http.ServeMux

	Panel *Panel

	registry *monkit.Registry
}

// NewServer returns a new debug.Server.
func NewServer(log *zap.Logger, listener net.Listener, registry *monkit.Registry, config Config) *Server {
	server := &Server{log: log}

	server.listener = listener
	server.server.Handler = &server.mux
	server.registry = registry

	server.Panel = NewPanel(log.Named("control"), "/control", config.ControlTitle)
	if config.Control {
		server.mux.Handle("/control/", server.Panel)
	}

	server.mux.Handle("/version/", http.StripPrefix("/version", newVersionHandler(log.Named("version"))))

	server.mux.HandleFunc("/debug/pprof/", pprof.Index)
	server.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	server.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	server.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	server.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	server.mux.HandleFunc("/debug/run/trace/db", server.collectTraces)

	server.mux.Handle("/mon/", http.StripPrefix("/mon", present.HTTP(server.registry)))
	server.mux.HandleFunc("/metrics", server.metrics)

	server.mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "OK")
	})

	return server
}

// Run starts the debug endpoint.
func (server *Server) Run(ctx context.Context) error {
	if server.listener == nil {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	var group errgroup.Group
	group.Go(func() error {
		<-ctx.Done()
		return Error.Wrap(server.server.Shutdown(context.Background()))
	})
	group.Go(func() error {
		defer cancel()
		return Error.Wrap(server.server.Serve(server.listener))
	})
	return group.Wait()
}

// Close closes server and underlying listener.
func (server *Server) Close() error {
	return Error.Wrap(server.server.Close())
}

// metrics writes https://prometheus.io/docs/instrumenting/exposition_formats/
func (server *Server) metrics(w http.ResponseWriter, r *http.Request) {
	// writes https://prometheus.io/docs/instrumenting/exposition_formats/
	// (https://prometheus.io/docs/concepts/metric_types/)
	server.registry.Stats(func(key monkit.SeriesKey, field string, val float64) {
		measurement := sanitize(key.Measurement)
		var metrics []string
		for tag, tagVal := range key.Tags.All() {
			metric := sanitize(tag) + "=\"" + sanitize(tagVal) + "\""
			metrics = append(metrics, metric)
		}
		fieldMetric := "field=\"" + sanitize(field) + "\""
		metrics = append(metrics, fieldMetric)

		_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n%s{"+
			strings.Join(metrics, ",")+"} %g\n", measurement, measurement, val)
	})
}

// collectTraces collects traces until request is canceled.
func (server *Server) collectTraces(w http.ResponseWriter, r *http.Request) {
	cancel := traces.CollectTraces()
	defer cancel()
	for {
		_, err := w.Write([]byte{0})
		if err != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// sanitize formats val to be suitable for prometheus.
func sanitize(val string) string {
	// https://prometheus.io/docs/concepts/data_model/
	// specifies all metric names must match [a-zA-Z_:][a-zA-Z0-9_:]*
	// Note: The colons are reserved for user defined recording rules.
	// They should not be used by exporters or direct instrumentation.
	if '0' <= val[0] && val[0] <= '9' {
		val = "_" + val
	}
	return strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z':
			return r
		case 'A' <= r && r <= 'Z':
			return r
		case '0' <= r && r <= '9':
			return r
		default:
			return '_'
		}
	}, val)
}

// VersionHandler implements version info endpoint.
type versionHandler struct {
	log *zap.Logger
}

// NewVersionHandler returns new version handler.
func newVersionHandler(log *zap.Logger) *versionHandler {
	return &versionHandler{log}
}

// ServeHTTP returns a json representation of the current version information for the binary.
func (handler *versionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	j, err := version.Build.Marshal()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_, err = w.Write(append(j, '\n'))
	if err != nil {
		handler.log.Sugar().Errorf("error writing data to client: %w", err)
	}
}
