package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// MessagesReceived counts platform messages that passed adapter filtering and reached the gRPC layer.
	MessagesReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "messaging_messages_received_total",
		Help: "Total messages received from platforms after adapter-level filtering.",
	}, []string{"platform"})

	// MessagesForwarded counts messages successfully sent to an agent stream.
	MessagesForwarded = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "messaging_messages_forwarded_total",
		Help: "Total messages successfully forwarded to an agent stream.",
	}, []string{"platform"})

	// MessagesDropped counts messages that were received but not forwarded.
	// reason: no_agent | allowlist | bot_filtered
	MessagesDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "messaging_messages_dropped_total",
		Help: "Total messages dropped before reaching an agent. Labelled by reason: no_agent, allowlist, bot_filtered.",
	}, []string{"platform", "reason"})

	// SlackEvents counts Slack events by interaction type before any filtering.
	// event_type: dm | thread_reply | mention | reaction
	SlackEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "messaging_slack_events_total",
		Help: "Total Slack events received, by type: dm, thread_reply, mention, reaction.",
	}, []string{"event_type"})

	// AgentResponses counts responses routed from agents, labelled by payload type.
	AgentResponses = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "messaging_agent_responses_total",
		Help: "Total agent responses routed to platform adapters.",
	}, []string{"type"})

	// ActiveStreams is the current number of open bidirectional gRPC agent streams.
	ActiveStreams = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "messaging_active_streams",
		Help: "Number of currently active bidirectional gRPC agent streams.",
	})

	// WebActiveConnections is the current number of open SSE client connections.
	WebActiveConnections = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "messaging_web_active_connections",
		Help: "Number of currently active SSE client connections.",
	})

	// RoutingErrors counts failures when routing responses back to adapters, labelled by adapter name.
	RoutingErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "messaging_routing_errors_total",
		Help: "Total errors encountered while routing agent responses to adapters.",
	}, []string{"adapter"})

	// MessageLatency measures the time from message receipt to successful agent forwarding.
	MessageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "messaging_message_latency_seconds",
		Help:    "Latency from message receipt to successful agent forwarding, in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"platform"})
)
