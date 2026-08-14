package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	signalsvc "nofx/service/signal"
)

func main() {
	cfg := signalsvc.LoadConfig()
	svc := signalsvc.NewService(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Ingest worker in the background.
	go svc.Start(ctx)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: signalsvc.NewHTTPHandler(svc),
	}

	go func() {
		log.Printf("signal-service listening on %s (interval=%s)", cfg.Listen, cfg.Interval)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
