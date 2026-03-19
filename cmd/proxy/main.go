// k8s-service-proxy is a Kubernetes service proxy sidecar for Docker Compose stacks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"

	"github.com/fealebenpae/kube-forwarding-proxy/app"
)

func main() {
	enableDNS := flag.Bool("dns", false, "Enable DNS server (default when no flags are given)")
	enableSocks := flag.Bool("socks", false, "Enable SOCKS5 proxy")
	flag.Parse()

	// Default behaviour: DNS only when the user passes no flags at all.
	if flag.NFlag() == 0 {
		*enableDNS = true
	}

	cfg, err := app.NewConfigFromEnvironment()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logger := buildLogger(cfg.LogLevel)
	defer func() { _ = logger.Sync() }()

	// Redirect client-go's klog output through the root zap logger.
	klog.SetLogger(zapr.NewLogger(logger.Desugar().Named("client-go")))

	logger.Infow("k8s-service-proxy starting",
		"interface", cfg.Interface,
		"vip_cidr", cfg.VIPCIDR,
		"cluster_domain", cfg.ClusterDomain,
		"dns_enabled", *enableDNS,
		"socks_enabled", *enableSocks,
		"http_listen", cfg.HTTPListen,
		"dns_listen", cfg.DNSListen,
		"socks_listen", cfg.SOCKSListen,
	)

	srv := app.NewServer(cfg, logger, *enableDNS, *enableSocks)
	if err := srv.Start(); err != nil {
		logger.Fatalw("failed to start server", "error", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logger.Infow("received signal, shutting down", "signal", sig.String())

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := srv.Stop(shutdownCtx); err != nil {
		logger.Errorw("shutdown error", "error", err)
	}

	logger.Info("k8s-service-proxy stopped")
}

func buildLogger(level string) *zap.SugaredLogger {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	encoderCfg := zap.NewDevelopmentEncoderConfig()
	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderCfg),
		zapcore.AddSync(os.Stdout),
		zap.NewAtomicLevelAt(zapLevel),
	)
	return zap.New(core).Sugar()
}
