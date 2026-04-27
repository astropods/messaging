package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/astropods/messaging/config"
	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/adapter/slack"
	"github.com/astropods/messaging/internal/adapter/web"
	"github.com/astropods/messaging/internal/authz"
	"github.com/astropods/messaging/internal/grpc"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// buildAuthorizer wires the per-deployment Authorizer used by adapters to
// gate incoming requests. Two states:
//   - ASTRO_AUTHZ_TOKEN set → real authz. The token's iss claim carries
//     astro-server's URL; no separate URL env var is needed.
//   - Empty → dev mode: AllowAll() so local development doesn't need
//     astro-server running.
//
// If the token is malformed (decode error, missing iss/sub), we fail
// closed with DenyAll() rather than silently widening access.
func buildAuthorizer(cfg config.AuthzConfig) authz.Authorizer {
	if cfg.IdentityToken == "" {
		slog.Warn("Authz disabled: ASTRO_AUTHZ_TOKEN not set (dev mode — all requests allowed)")
		return authz.AllowAll()
	}
	a, err := authz.NewAuthorizer(authz.Config{IdentityToken: cfg.IdentityToken})
	if err != nil {
		slog.Error("Failed to initialize authorizer; failing closed", "err", err)
		return authz.DenyAll()
	}
	return a
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	slog.Info("Starting Astro Messaging Service...")
	slog.Info(version.Info())

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		slog.Error("Failed to load configuration", "err", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// Initialize conversation store
	var conversationStore store.ConversationStore
	if cfg.Storage.Type == "redis" {
		slog.Info("Initializing Redis store", "url", cfg.Storage.RedisURL)
		redisStore, err := store.NewRedisStore(cfg.Storage.RedisURL, cfg.Storage.TTL)
		if err != nil {
			slog.Error("Failed to initialize Redis store", "err", err)
			os.Exit(1)
		}
		conversationStore = redisStore
		defer func() {
			if err := redisStore.Close(); err != nil {
				slog.Error("Error closing Redis store", "err", err)
			}
		}()
		slog.Info("Redis store initialized")
	} else {
		slog.Info("Using in-memory conversation store (data will not persist)")
		conversationStore = store.NewMemoryStore()
	}

	// Initialize thread history store
	threadStore := store.NewThreadHistoryStore(
		cfg.ThreadHistory.MaxSize,
		cfg.ThreadHistory.MaxMessages,
		time.Duration(cfg.ThreadHistory.TTL)*time.Hour,
	)
	slog.Info("Thread history store initialized",
		"max_size", cfg.ThreadHistory.MaxSize,
		"max_messages", cfg.ThreadHistory.MaxMessages,
		"ttl", cfg.ThreadHistory.TTL)

	// Initialize agent config store
	agentConfigStore := store.NewAgentConfigStore()

	// Initialize gRPC server (if enabled)
	var grpcServer *grpc.Server
	if cfg.GRPC.Enabled {
		slog.Info("Initializing gRPC server...")
		grpcServer = grpc.NewServer(cfg.GRPC.ListenAddr, threadStore, conversationStore, agentConfigStore)
		slog.Info("gRPC server initialized", "addr", cfg.GRPC.ListenAddr)
	}

	// Build the authorizer once per process and share it across adapters.
	// Adapters check Allowed() at request ingress; the cache lives inside
	// the Authorizer so all adapters share one cache for the deployment.
	authorizer := buildAuthorizer(cfg.Authz)

	// Initialize adapters
	adapters := initializeAdapters(ctx, cfg, threadStore, agentConfigStore, authorizer)
	if len(adapters) == 0 && !cfg.GRPC.Enabled {
		slog.Error("No adapters enabled or configured and gRPC is disabled")
		os.Exit(1)
	}
	if len(adapters) == 0 {
		slog.Info("No platform adapters configured - running in gRPC-only mode")
	}

	// Register adapters with gRPC server
	if grpcServer != nil && len(adapters) > 0 {
		for name, adpt := range adapters {
			slog.Info("Registering adapter with gRPC server", "adapter", name)
			grpcServer.RegisterAdapter(name, adpt)
		}

		// Register gRPC message handler with adapters
		// When messages arrive from platforms, route them to agent via gRPC
		for name, adpt := range adapters {
			slog.Info("Registering gRPC message handler for adapter", "adapter", name)
			adpt.SetMessageHandler(grpcServer.HandleIncomingMessage)

			// Wire audio forwarder for adapters that support it
			if wa, ok := adpt.(*web.WebAdapter); ok {
				wa.SetAudioForwarder(grpcServer)
				slog.Info("Registered audio forwarder for adapter", "adapter", name)
			}
		}
	}

	// Start metrics server
	if cfg.Metrics.Enabled {
		go func() {
			mux := http.NewServeMux()
			mux.Handle("/metrics", promhttp.Handler())
			srv := &http.Server{
				Addr:        cfg.Metrics.ListenAddr,
				Handler:     mux,
				ReadTimeout: 5 * time.Second,
				IdleTimeout: 60 * time.Second,
			}
			slog.Info("Starting metrics server", "addr", cfg.Metrics.ListenAddr)
			if err := srv.ListenAndServe(); err != nil {
				slog.Error("Metrics server error", "err", err)
			}
		}()
	}

	// Start gRPC server
	if grpcServer != nil {
		go func() {
			slog.Info("Starting gRPC server", "addr", cfg.GRPC.ListenAddr)
			if err := grpcServer.Start(ctx); err != nil {
				slog.Error("gRPC server error", "err", err)
			}
		}()
	}

	// Start all adapters
	if len(adapters) > 0 {
		for name, adapterInstance := range adapters {
			go func(n string, a adapter.Adapter) {
				slog.Info("Starting adapter", "adapter", n)
				if err := a.Start(ctx); err != nil {
					slog.Error("Error starting adapter", "adapter", n, "err", err)
				}
			}(name, adapterInstance)
		}
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	slog.Info("Shutting down gracefully...")

	// Stop gRPC server
	if grpcServer != nil {
		slog.Info("Stopping gRPC server...")
		grpcServer.Stop()
	}

	// Stop all adapters
	for name, adapterInstance := range adapters {
		slog.Info("Stopping adapter", "adapter", name)
		if err := adapterInstance.Stop(ctx); err != nil {
			slog.Error("Error stopping adapter", "adapter", name, "err", err)
		}
	}

	// Close conversation store
	if err := conversationStore.Close(); err != nil {
		slog.Error("Error closing conversation store", "err", err)
	}

	slog.Info("Shutdown complete")
}

// initializeAdapters creates and initializes adapters based on configuration
func initializeAdapters(ctx context.Context, cfg *config.Config, threadStore *store.ThreadHistoryStore, agentConfigStore *store.AgentConfigStore, authorizer authz.Authorizer) map[string]adapter.Adapter {
	adapters := make(map[string]adapter.Adapter)

	// Initialize Slack adapter if enabled
	if cfg.Slack.Enabled {
		slog.Info("Initializing Slack adapter...")
		slackAdapter := slack.New()
		if err := slackAdapter.Initialize(ctx, cfg.Slack.Config); err != nil {
			slog.Error("Error initializing Slack adapter", "err", err)
		} else {
			slackAdapter.SetAuthorizer(authorizer)
			adapters["slack"] = slackAdapter
			slog.Info("Slack adapter initialized")
		}
	}

	// Initialize Web adapter if enabled
	if cfg.Web.Enabled {
		slog.Info("Initializing Web adapter...")
		// In production astro-server runs the web adapter behind ALB OIDC,
		// which injects the WorkOS user ID as x-amzn-oidc-identity. Read
		// that header as the session userID; without it (local dev) the
		// adapter falls back to NoopSessionManager via the option below.
		webOpts := []web.WebAdapterOption{
			web.WithListenAddr(cfg.Web.ListenAddr),
			web.WithAllowedOrigins(cfg.Web.AllowedOrigins),
			web.WithServePlayground(cfg.Web.ServePlayground),
		}
		if cfg.Authz.IdentityToken != "" {
			webOpts = append(webOpts, web.WithSessionManager(
				web.NewHeaderSessionManager("x-amzn-oidc-identity", "", ""),
			))
		}
		webAdapter := web.New(webOpts...)
		if err := webAdapter.Initialize(ctx, adapter.Config{}); err != nil {
			slog.Error("Error initializing Web adapter", "err", err)
		} else {
			webAdapter.SetThreadStore(threadStore)
			webAdapter.SetAgentConfigStore(agentConfigStore)
			webAdapter.SetAuthorizer(authorizer)
			adapters["web"] = webAdapter
			slog.Info("Web adapter initialized")
		}
	}

	return adapters
}
