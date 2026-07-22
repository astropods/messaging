package grpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

// invokeAPI is the slice of the bedrockagentcore client the sigv4 invoker needs.
// Declaring it as an interface lets unit tests inject a fake (no live AWS, no
// credentials) while production uses the real *bedrockagentcore.Client.
type invokeAPI interface {
	InvokeAgentRuntime(ctx context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput,
		optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
}

// sigv4Invoker reaches a REAL AgentCore runtime over the bedrock-agentcore
// data-plane API. There is no raw HTTP URL for a deployed runtime — the only way
// in is InvokeAgentRuntime(arn, sessionId, payload), which the SDK SigV4-signs
// with the ambient AWS credentials. The runtime's container streams its SSE bytes
// straight back through Output.Response (an io.ReadCloser), so the transport's
// existing SSE scanner consumes it unchanged.
//
// When messaging runs inside a VPC that has the com.amazonaws.<region>.bedrock-
// agentcore interface endpoint, this same call resolves to that endpoint's
// private DNS and never traverses the public internet (INV3) — a deploy-time
// concern, not a code difference.
type sigv4Invoker struct {
	api       invokeAPI
	arn       string
	qualifier string // optional endpoint/version; "" ⇒ default endpoint
}

func (s *sigv4Invoker) Describe() string { return "sigv4:" + s.arn }

// NewSigV4Invoker builds the signed AWS backend for a runtime ARN. It loads the
// default AWS config (env/shared-profile/instance role) once; region falls back
// to the config chain if empty. Returns an error if AWS config can't be loaded.
func NewSigV4Invoker(ctx context.Context, arn, region, qualifier string) (AgentInvoker, error) {
	if arn == "" {
		return nil, fmt.Errorf("agentcore sigv4 invoker: AGENT_RUNTIME_ARN is required")
	}
	var optFns []func(*awsconfig.LoadOptions) error
	if region != "" {
		optFns = append(optFns, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return &sigv4Invoker{
		api:       bedrockagentcore.NewFromConfig(cfg),
		arn:       arn,
		qualifier: qualifier,
	}, nil
}

func (s *sigv4Invoker) Invoke(ctx context.Context, sessionID string, payload []byte) (io.ReadCloser, error) {
	in := &bedrockagentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn:  aws.String(s.arn),
		RuntimeSessionId: aws.String(runtimeSessionID(sessionID)),
		Payload:          payload,
		ContentType:      aws.String("application/json"),
		Accept:           aws.String("text/event-stream"),
	}
	if s.qualifier != "" {
		in.Qualifier = aws.String(s.qualifier)
	}
	out, err := s.api.InvokeAgentRuntime(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("InvokeAgentRuntime: %w", err)
	}
	return out.Response, nil
}

// runtimeSessionID adapts an Astro conversationID to AgentCore's session-id
// contract: 33–256 chars. Conversation ids are often shorter (e.g. a 16-char
// nanoid), which the API rejects. We keep the mapping 1:1 and deterministic
// (INV2) by prefixing a stable tag and, when still short, padding with a
// SHA-256 digest of the id — same conversation ⇒ same session ⇒ warm microVM.
func runtimeSessionID(conversationID string) string {
	const prefix = "astro-conv-"
	id := prefix + conversationID
	if len(id) >= 33 {
		return clampSession(id)
	}
	sum := sha256.Sum256([]byte(conversationID))
	id += "-" + hex.EncodeToString(sum[:]) // + up to 64 hex chars, always >=33 now
	return clampSession(id)
}

func clampSession(id string) string {
	if len(id) > 256 {
		return id[:256]
	}
	return id
}
