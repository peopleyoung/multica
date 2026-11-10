// native-jobs is an independently supervised command job service. It does not
// connect to the Multica database or load model credentials.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/tools"
	"github.com/multica-ai/multica/server/internal/jobservice"
)

func main() {
	configPath := flag.String("config", "", "private JSON configuration file (0600)")
	flag.Parse()
	if err := run(*configPath); err != nil {
		log.Fatal(err)
	}
}

func run(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("open job service configuration: specify --config")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("job service configuration must be a private regular file (0600)")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(raw) > 65536 {
		return errors.New("job service configuration exceeds limit")
	}
	cfg := jobservice.Config{Listen: "127.0.0.1:23110", Limits: tools.ServiceLimits{MaxActive: 4, MaxRecords: 4096, LogBytes: 64 << 20}}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid job service configuration")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || host == "" {
		return errors.New("job service listen address requires an explicit host and port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("invalid job service listen port")
	}
	service, err := jobservice.New(cfg, os.Environ())
	if err != nil {
		return err
	}
	defer service.Close()
	server := &http.Server{Addr: cfg.Listen, Handler: service, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		// Stop service jobs first so requests waiting on their settlement can
		// complete before the HTTP shutdown deadline.
		_ = service.Close()
		return server.Shutdown(shutdown)
	}
	return nil
}
