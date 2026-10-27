package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/thatguy-za/vigil/internal/api"
	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/hw"
	"github.com/thatguy-za/vigil/internal/live"
	"github.com/thatguy-za/vigil/internal/recorder"
	"github.com/thatguy-za/vigil/internal/storage"
)

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfgDir, recDir := env("VIGIL_CONFIG", "/config"), env("VIGIL_RECORDINGS", "/recordings")
	for _, d := range []string{cfgDir, recDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	d, err := db.Open(cfgDir)
	if err != nil {
		log.Fatal(err)
	}
	info := hw.Detect()
	log.Printf("vigil %s on %s (%d MB, %d cores), encoder: %s (hardware=%v)",
		version, info.Model, info.MemMB, info.Cores, info.Encoder.Name, info.Encoder.Hardware)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	rec := recorder.New(d, recDir, info.Encoder)
	go rec.Run(ctx)
	go storage.Run(ctx, d, recDir)

	srv := &http.Server{Addr: ":" + env("VIGIL_PORT", "8080"), ReadHeaderTimeout: 10 * time.Second,
		Handler: (&api.Server{DB: d, Rec: rec, Live: live.New(ctx), HW: info, RecDir: recDir, Version: version}).Handler()}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}
	time.Sleep(time.Second) // let the recorder finalise its segments
}
