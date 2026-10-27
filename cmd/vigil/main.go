package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/thatguy-za/vigil/internal/api"
	"github.com/thatguy-za/vigil/internal/auth"
	"github.com/thatguy-za/vigil/internal/db"
	"github.com/thatguy-za/vigil/internal/discover"
	"github.com/thatguy-za/vigil/internal/hw"
	"github.com/thatguy-za/vigil/internal/live"
	"github.com/thatguy-za/vigil/internal/recorder"
	"github.com/thatguy-za/vigil/internal/storage"
	"golang.org/x/term"
)

var version = "dev"

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	cfgDir, recDir, bufDir := env("VIGIL_CONFIG", "/config"), env("VIGIL_RECORDINGS", "/recordings"), env("VIGIL_BUFFER", "/tmp/vigil")
	if len(os.Args) > 1 {
		resetPassword(cfgDir, os.Args[1:])
		return
	}
	for _, d := range []string{cfgDir, recDir, bufDir} {
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
	rec := recorder.New(d, recDir, bufDir, info.Encoder, info.HWDecode)
	go rec.Run(ctx)
	go storage.Run(ctx, d, recDir)
	scanner := &discover.Scanner{DB: d}
	go scanner.Run(ctx)

	srv := &http.Server{Addr: ":" + env("VIGIL_PORT", "8080"), ReadHeaderTimeout: 10 * time.Second,
		Handler: (&api.Server{DB: d, Rec: rec, Live: live.New(ctx), Scan: scanner, HW: info, RecDir: recDir, Version: version}).Handler()}
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

// resetPassword implements `vigil reset-password <username>` for admins who are locked out.
func resetPassword(cfgDir string, args []string) {
	if args[0] != "reset-password" || len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: vigil reset-password <username>")
		os.Exit(2)
	}
	d, err := db.Open(cfgDir)
	if err != nil {
		log.Fatal(err)
	}
	u, err := d.UserByName(args[1])
	if err != nil {
		log.Fatalf("no such user: %s", args[1])
	}
	var pw string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Print("New password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			log.Fatal(err)
		}
		pw = string(b)
	} else {
		pw, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimRight(pw, "\r\n")
	}
	if len(pw) < 8 {
		log.Fatal("password must be at least 8 characters")
	}
	if err := d.SetPassword(u.ID, auth.Hash(pw)); err != nil {
		log.Fatal(err)
	}
	d.DeleteUserSessions(u.ID, "")
	fmt.Printf("Password updated for %s. They have been signed out everywhere.\n", u.Username)
}
