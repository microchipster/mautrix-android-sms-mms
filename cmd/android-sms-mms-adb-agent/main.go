// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

// android-sms-mms-adb-agent is the phone host side of the mautrix-android-sms-mms
// bridge: it reads SMS/MMS from a USB-connected Android device over adb and
// exposes them as a local HTTP API to the bridge.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	agent "mautrix-android-sms-mms/internal/agent"
)

var version = "0.1.0-dev"

func main() {
	configPath := flag.String("c", "agent.yaml", "Path to the agent config file")
	listen := flag.String("listen", "", "Override listen address (default from config)")
	token := flag.String("token", "", "Override bearer token")
	serial := flag.String("serial", "", "Override adb device serial")
	adbPath := flag.String("adb", "", "Override path to the adb binary")
	pollMS := flag.Int("poll-ms", 0, "Override poll interval in milliseconds")
	printExample := flag.Bool("e", false, "Print an example config file and exit")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *printExample {
		fmt.Print(agent.ExampleConfig())
		return
	}
	if *showVersion {
		fmt.Println("android-sms-mms-adb-agent", version)
		return
	}

	zlog := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339}).With().Timestamp().Logger()

	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		zlog.Fatal().Err(err).Str("path", *configPath).Msg("Failed to load config")
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *serial != "" {
		cfg.Serial = *serial
	}
	if *adbPath != "" {
		cfg.AdbPath = *adbPath
	}
	if *pollMS > 0 {
		cfg.PollIntervalMS = *pollMS
	}
	if cfg.Token == "" && !isLoopbackListen(cfg.Listen) {
		zlog.Fatal().Str("listen", cfg.Listen).Msg("Refusing to listen on a non-loopback address without a token")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	adb := agent.NewAdb(cfg.AdbPath, cfg.Serial)
	device, err := adb.DeviceInfo(ctx)
	if err != nil {
		zlog.Fatal().Err(err).Msg("Failed to query device over adb (is it plugged in and authorized?)")
	}
	root := adb.IsRoot(ctx)
	defaultSMSApp := detectDefaultSMSApp(ctx, adb)

	prov := agent.NewContentProvider(adb, root, cfg.OwnNumbers, cfg.SubIDs)
	state, err := agent.OpenState(cfg.DataDir)
	if err != nil {
		zlog.Fatal().Err(err).Str("dir", cfg.DataDir).Msg("Failed to open state")
	}
	eventLog := agent.NewEventLog(20000)

	sweeper := agent.NewSweeper(cfg, prov, state, eventLog, zlog)
	go sweeper.Run(ctx)

	sender := agent.NewSender(cfg, adb, prov, state, device.AndroidSDK, zlog)

	subscriptions, err := prov.Subscriptions(ctx)
	if err != nil {
		zlog.Warn().Err(err).Msg("Failed to read SIM subscriptions")
	}

	capabilities := []string{"threads", "history", "media", "read_state"}
	if sender.Enabled() {
		capabilities = append(capabilities, "send_sms")
	}
	if sender.SupportsMMS() {
		capabilities = append(capabilities, "send_mms")
	}

	sendBackend := ""
	if sender.Enabled() {
		sendBackend = "shell"
	}
	health := agent.Health{
		OK:              true,
		ProtocolVersion: agent.ProtocolVersion,
		Device:          device,
		Root:            root,
		Backend: agent.BackendInfo{
			Read:          "content-provider",
			SendSMS:       sendBackend,
			SendMMS:       sender.SupportsMMS(),
			CanMarkRead:   true,
			Root:          root,
			DefaultSMSApp: defaultSMSApp,
		},
		DefaultSMSApp: defaultSMSApp,
		Subscriptions: subscriptions,
		Capabilities:  capabilities,
	}

	server := agent.NewServer(cfg, prov, state, eventLog, sender, health, zlog)
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	zlog.Info().
		Str("listen", cfg.Listen).
		Str("serial", device.Serial).
		Str("model", device.Model).
		Int("sdk", device.AndroidSDK).
		Bool("root", root).
		Str("default_sms_app", defaultSMSApp).
		Bool("send_sms", sender.Enabled()).
		Msg("android-sms-mms-adb-agent started")

	serveErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		zlog.Error().Err(err).Msg("HTTP server failed")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := state.Save(); err != nil {
		zlog.Warn().Err(err).Msg("Failed to save state on shutdown")
	}
	zlog.Info().Msg("android-sms-mms-adb-agent stopped")
}

func isLoopbackListen(listen string) bool {
	return strings.HasPrefix(listen, "127.0.0.1") || strings.HasPrefix(listen, "localhost") || strings.HasPrefix(listen, "[::1]")
}

// detectDefaultSMSApp asks the role manager which package holds the SMS role.
func detectDefaultSMSApp(ctx context.Context, adb *agent.Adb) string {
	out, err := adb.Shell(ctx, "cmd role get-role-holders --user 0 android.app.role.SMS")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, ".") && !strings.Contains(line, " ") {
			return line
		}
	}
	return ""
}
