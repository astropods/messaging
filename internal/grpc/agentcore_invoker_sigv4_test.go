package grpc

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
)

// fakeInvokeAPI records the last input and returns a canned SSE stream, so the
// sigv4 invoker is exercised with no AWS credentials or network.
type fakeInvokeAPI struct {
	last *bedrockagentcore.InvokeAgentRuntimeInput
	sse  string
}

func (f *fakeInvokeAPI) InvokeAgentRuntime(_ context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput,
	_ ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	f.last = in
	return &bedrockagentcore.InvokeAgentRuntimeOutput{
		Response: io.NopCloser(strings.NewReader(f.sse)),
	}, nil
}

func TestSigV4Invoker_PassesArnPayloadAndStreamsResponse(t *testing.T) {
	fake := &fakeInvokeAPI{sse: "data: {\"type\":\"START\"}\ndata: {\"type\":\"END\"}\n"}
	inv := &sigv4Invoker{api: fake, arn: "arn:aws:bedrock-agentcore:us-east-1:123:runtime/x"}

	body := []byte(`{"prompt":"hi","sessionId":"conv-1"}`)
	rc, err := inv.Invoke(context.Background(), "conv-1", body)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	defer rc.Close()

	if got := aws.ToString(fake.last.AgentRuntimeArn); got != inv.arn {
		t.Errorf("ARN = %q, want %q", got, inv.arn)
	}
	if string(fake.last.Payload) != string(body) {
		t.Errorf("Payload = %q, want %q", fake.last.Payload, body)
	}
	if got := aws.ToString(fake.last.ContentType); got != "application/json" {
		t.Errorf("ContentType = %q", got)
	}
	out, _ := io.ReadAll(rc)
	if !strings.Contains(string(out), `"type":"START"`) {
		t.Errorf("response not streamed through: %q", out)
	}
}

func TestSigV4Invoker_QualifierOptional(t *testing.T) {
	fake := &fakeInvokeAPI{sse: "data: {\"type\":\"END\"}\n"}
	inv := &sigv4Invoker{api: fake, arn: "arn:x"} // no qualifier
	if _, err := inv.Invoke(context.Background(), "c", nil); err != nil {
		t.Fatal(err)
	}
	if fake.last.Qualifier != nil {
		t.Errorf("Qualifier should be nil when unset, got %q", aws.ToString(fake.last.Qualifier))
	}

	inv2 := &sigv4Invoker{api: fake, arn: "arn:x", qualifier: "PROD"}
	if _, err := inv2.Invoke(context.Background(), "c", nil); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(fake.last.Qualifier); got != "PROD" {
		t.Errorf("Qualifier = %q, want PROD", got)
	}
}

func TestRuntimeSessionID_ContractLength(t *testing.T) {
	cases := []string{"", "c", "conv-1", "short", strings.Repeat("x", 20), strings.Repeat("y", 400)}
	for _, in := range cases {
		got := runtimeSessionID(in)
		if len(got) < 33 {
			t.Errorf("runtimeSessionID(%q) len=%d, want >=33", in, len(got))
		}
		if len(got) > 256 {
			t.Errorf("runtimeSessionID(%q) len=%d, want <=256", in, len(got))
		}
	}
}

func TestRuntimeSessionID_DeterministicAnd1to1(t *testing.T) {
	// Same conversation ⇒ same session (INV2: keeps the microVM warm).
	if runtimeSessionID("conv-42") != runtimeSessionID("conv-42") {
		t.Error("not deterministic for the same conversation id")
	}
	// Different conversations ⇒ different sessions.
	if runtimeSessionID("conv-a") == runtimeSessionID("conv-b") {
		t.Error("collision between distinct conversation ids")
	}
}
