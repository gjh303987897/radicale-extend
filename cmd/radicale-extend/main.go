package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"radicale-extend/internal/config"
	"radicale-extend/internal/mcpapi"
	"radicale-extend/internal/service"
	"radicale-extend/internal/store"
	"radicale-extend/internal/telegrambot"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	mode := "serve"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	path := flags.String("config", "config.yaml", "configuration path")
	address := flags.String("url", "http://127.0.0.1:8080/readyz", "health endpoint")
	if len(os.Args) > 1 {
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
	}
	if mode == "healthcheck" {
		client := http.Client{Timeout: 3 * time.Second}
		r, err := client.Get(*address)
		if err != nil {
			return errors.New("health endpoint unreachable")
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return errors.New("service not ready")
		}
		return nil
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	switch mode {
	case "check-config":
		fmt.Println("configuration valid")
		return nil
	case "init-db":
		db, err := store.Open(c.Storage.State, c.Storage.Queue)
		if err == nil {
			db.Close()
		}
		return err
	case "serve", "mcp-stdio":
	default:
		return errors.New("command must be serve, mcp-stdio, check-config, init-db or healthcheck")
	}
	if mode == "serve" && len(c.Token.Value) < 32 {
		return errors.New("HTTP MCP token must contain at least 32 bytes")
	}
	s, err := service.New(c)
	if err != nil {
		return err
	}
	defer s.Store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := mcpapi.New(s)
	if mode == "mcp-stdio" {
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); s.Run(ctx) }()
	botDone := make(chan struct{})
	go func() { defer close(botDone); telegrambot.New(s).Run(ctx) }()
	httpServer := &http.Server{Addr: c.Listen, Handler: mcpapi.HTTP(s, server), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	failure := make(chan error, 1)
	go func() { failure <- httpServer.ListenAndServe() }()
	slog.Info("server started", "listen", c.Listen)
	select {
	case <-ctx.Done():
	case err = <-failure:
		cancel()
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 25*time.Second)
	defer stop()
	shutdownErr := httpServer.Shutdown(shutdownCtx)
	cancel()
	<-workerDone
	<-botDone
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return shutdownErr
}
