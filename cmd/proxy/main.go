// k8s-service-proxy is a Kubernetes service proxy.
//
// Subcommands:
//
//	k8s-service-proxy             — run the daemon (DNS server by default)
//	k8s-service-proxy install     — privileged macOS host setup
//	k8s-service-proxy uninstall   — reverse the install
//	k8s-service-proxy status      — show install + daemon state
//	k8s-service-proxy register    — register a kubeconfig with the daemon
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"

	"github.com/fealebenpae/kube-forwarding-proxy/app"
)

func main() {
	args := os.Args[1:]
	sub, rest := splitSubcommand(args)

	switch sub {
	case "install":
		os.Exit(runInstall(rest))
	case "uninstall":
		os.Exit(runUninstall(rest))
	case "status":
		os.Exit(runStatus(rest))
	case "register":
		os.Exit(runRegister(rest))
	default:
		// No subcommand → daemon. Prepend the unconsumed args back to flag.CommandLine
		// so `--dns` / `--socks` parse the same as before.
		os.Args = append([]string{os.Args[0]}, rest...)
		runDaemon()
	}
}

// splitSubcommand returns the first positional argument as the subcommand, and
// the remaining args (everything before the subcommand-position arg, plus
// everything after it) for the subcommand to parse. When the first arg starts
// with "-" or is empty, sub is "" and rest is args unchanged.
func splitSubcommand(args []string) (sub string, rest []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func runDaemon() {
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
