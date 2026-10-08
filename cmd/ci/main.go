package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/MeteorsLiu/cibot/internal/bot"
	"github.com/joho/godotenv"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: ci serve")
		os.Exit(2)
	}

	if err := godotenv.Load(); err != nil {
		log.Fatal("load .env: ", err)
	}
	appID, err := strconv.ParseInt(os.Getenv("APP_ID"), 10, 64)
	if err != nil {
		log.Fatal("APP_ID: ", err)
	}
	handler, err := bot.New(os.Getenv("WEBHOOK_SECRET"), appID, os.Getenv("PRIVATE_KEY_FILE"))
	if err != nil {
		log.Fatal("initialize bot: ", err)
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":80"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal("listen: ", err)
	}
	server := &http.Server{Handler: handler}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		if err := server.Shutdown(context.Background()); err != nil {
			log.Print("shutdown: ", err)
		}
	}()
	log.Printf("ci: listening on %s", listener.Addr())
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal("serve: ", err)
	}
	<-done
}
