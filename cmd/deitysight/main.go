package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/0x0034/deitysight/internal/agent"
	"github.com/0x0034/deitysight/internal/sandbox"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	config := flag.String("config", "/etc/deitysight/agent.yaml", "configuration file")
	version := flag.Bool("version", false, "print version")
	flag.Parse()
	if *version {
		fmt.Println(agent.Version)
		return nil
	}
	if runtime.GOOS != "linux" {
		return errors.New("deitysight requires Linux")
	}
	if os.Geteuid() != 0 {
		return errors.New("deitysight must run as root")
	}
	cfg, err := agent.LoadConfig(*config)
	if err != nil {
		return err
	}
	if err := sandbox.RestrictProcessControl(); err != nil {
		return err
	}
	collector, err := agent.NewLinuxCollector("/proc", cfg.Sampling.MaxSourceBytes)
	if err != nil {
		return err
	}
	defer collector.Close()
	a, err := agent.New(cfg, collector)
	if err != nil {
		return err
	}
	defer a.Close()
	srv := &http.Server{Addr: cfg.HTTP.Listen, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serve := make(chan error, 1)
	go func() { serve <- srv.ListenAndServe() }()
	log.Printf("deitysight %s listening on %s", agent.Version, cfg.HTTP.Listen)
	select {
	case err = <-serve:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if srv.Shutdown(shutdown) != nil {
			_ = srv.Close()
		}
	}
	return nil
}
