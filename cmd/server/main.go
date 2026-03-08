package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/astropods/messaging/config"
	"github.com/astropods/messaging/internal/adapter"
	"github.com/astropods/messaging/internal/adapter/slack"
	"github.com/astropods/messaging/internal/adapter/web"
	"github.com/astropods/messaging/internal/grpc"
	"github.com/astropods/messaging/internal/store"
	"github.com/astropods/messaging/internal/version"
)

func main() {
	log.Println("Starting Astro Messaging Service...")
	log.Println(version.Info())

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	ctx := context.Background()

	// Initialize conversation store
	var conversationStore store.ConversationStore
	if cfg.Storage.Type == "redis" {
		log.Printf("Initializing Redis store: %s", cfg.Storage.RedisURL)
		redisStore, err := store.NewRedisStore(cfg.Storage.RedisURL, cfg.Storage.TTL)
		if err != nil {
			log.Fatalf("Failed to initialize Redis store: %v", err)
		}
		conversationStore = redisStore
		defer redisStore.Close()
		log.Println("Redis store initialized")
	} else {
		log.Println("Using in-memory conversation store (data will not persist)")
		conversationStore = store.NewMemoryStore()
	}

	// Initialize thread history store
	threadStore := store.NewThreadHistoryStore(
		cfg.ThreadHistory.MaxSize,
		cfg.ThreadHistory.MaxMessages,
		time.Duration(cfg.ThreadHistory.TTL)*time.Hour,
	)
	log.Printf("Thread history store initialized (max_size=%d, max_messages=%d, ttl=%dh)",
		cfg.ThreadHistory.MaxSize, cfg.ThreadHistory.MaxMessages, cfg.ThreadHistory.TTL)

	// Initialize agent config store
	agentConfigStore := store.NewAgentConfigStore()

	// Initialize gRPC server (if enabled)
	var grpcServer *grpc.Server
	if cfg.GRPC.Enabled {
		log.Println("Initializing gRPC server...")
		grpcServer = grpc.NewServer(cfg.GRPC.ListenAddr, threadStore, conversationStore, agentConfigStore)
		log.Printf("gRPC server initialized on %s", cfg.GRPC.ListenAddr)
	}

	// Initialize adapters
	adapters := initializeAdapters(ctx, cfg, threadStore, agentConfigStore)
	if len(adapters) == 0 && !cfg.GRPC.Enabled {
		log.Fatal("No adapters enabled or configured and gRPC is disabled")
	}
	if len(adapters) == 0 {
		log.Println("No platform adapters configured - running in gRPC-only mode")
	}

	// Register adapters with gRPC server
	if grpcServer != nil && len(adapters) > 0 {
		for name, adpt := range adapters {
			log.Printf("Registering %s adapter with gRPC server...", name)
			grpcServer.RegisterAdapter(name, adpt)
		}

		// Register gRPC message handler with adapters
		// When messages arrive from platforms, route them to agent via gRPC
		for name, adpt := range adapters {
			log.Printf("Registering gRPC message handler for %s adapter...", name)
			adpt.SetMessageHandler(grpcServer.HandleIncomingMessage)

			// Wire audio handler for adapters that support it
			if wa, ok := adpt.(*web.WebAdapter); ok {
				wa.SetAudioHandler(grpcServer.HandleIncomingAudio)
				log.Printf("Registered audio handler for %s adapter", name)
			}
		}
	}

	// Start gRPC server
	if grpcServer != nil {
		go func() {
			log.Printf("Starting gRPC server on %s", cfg.GRPC.ListenAddr)
			if err := grpcServer.Start(ctx); err != nil {
				log.Printf("gRPC server error: %v", err)
			}
		}()
	}

	// Start all adapters
	if len(adapters) > 0 {
		for name, adapterInstance := range adapters {
			go func(n string, a adapter.Adapter) {
				log.Printf("Starting %s adapter...", n)
				if err := a.Start(ctx); err != nil {
					log.Printf("Error starting %s adapter: %v", n, err)
				}
			}(name, adapterInstance)
		}
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down gracefully...")

	// Stop gRPC server
	if grpcServer != nil {
		log.Println("Stopping gRPC server...")
		grpcServer.Stop()
	}

	// Stop all adapters
	for name, adapterInstance := range adapters {
		log.Printf("Stopping %s adapter...", name)
		if err := adapterInstance.Stop(ctx); err != nil {
			log.Printf("Error stopping %s adapter: %v", name, err)
		}
	}

	// Close conversation store
	if err := conversationStore.Close(); err != nil {
		log.Printf("Error closing conversation store: %v", err)
	}

	log.Println("Shutdown complete")
}

// initializeAdapters creates and initializes adapters based on configuration
func initializeAdapters(ctx context.Context, cfg *config.Config, threadStore *store.ThreadHistoryStore, agentConfigStore *store.AgentConfigStore) map[string]adapter.Adapter {
	adapters := make(map[string]adapter.Adapter)

	// Initialize Slack adapter if enabled
	if cfg.Slack.Enabled {
		log.Println("Initializing Slack adapter...")
		slackAdapter := slack.New()
		if err := slackAdapter.Initialize(ctx, cfg.Slack.Config); err != nil {
			log.Printf("Error initializing Slack adapter: %v", err)
		} else {
			adapters["slack"] = slackAdapter
			log.Println("Slack adapter initialized")
		}
	}

	// Initialize Web adapter if enabled
	if cfg.Web.Enabled {
		log.Println("Initializing Web adapter...")
		webAdapter := web.New(
			web.WithListenAddr(cfg.Web.ListenAddr),
			web.WithAllowedOrigins(cfg.Web.AllowedOrigins),
		)
		if err := webAdapter.Initialize(ctx, adapter.Config{}); err != nil {
			log.Printf("Error initializing Web adapter: %v", err)
		} else {
			webAdapter.SetThreadStore(threadStore)
			webAdapter.SetAgentConfigStore(agentConfigStore)
			adapters["web"] = webAdapter
			log.Println("Web adapter initialized")
		}
	}

	return adapters
}
