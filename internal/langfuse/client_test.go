package langfuse

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_Disabled_NoOp(t *testing.T) {
	c := New("", "", "")
	if c.Enabled() {
		t.Fatalf("Enabled() = true for empty creds; want false")
	}
	if err := c.CreateScore(context.Background(), ScoreRequest{TraceID: "t", Name: "n", Value: 1}); err != nil {
		t.Fatalf("disabled CreateScore returned error: %v", err)
	}
}

func TestClient_NilReceiver_NoOp(t *testing.T) {
	var c *Client
	if c.Enabled() {
		t.Fatalf("Enabled() = true on nil receiver")
	}
	if err := c.CreateScore(context.Background(), ScoreRequest{}); err != nil {
		t.Fatalf("nil receiver CreateScore returned error: %v", err)
	}
}

func TestClient_CreateScore_PostsToScoresEndpoint(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody ScoreRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := New(srv.URL, "pk", "sk")
	err := c.CreateScore(context.Background(), ScoreRequest{
		ID:      "ts-123-user-feedback",
		TraceID: "trace-abc",
		Name:    "user-feedback",
		Value:   1,
	})
	if err != nil {
		t.Fatalf("CreateScore: %v", err)
	}
	if gotPath != "/api/public/scores" {
		t.Fatalf("path = %q; want /api/public/scores", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("Authorization header = %q; want Basic prefix", gotAuth)
	}
	if gotBody.TraceID != "trace-abc" || gotBody.Name != "user-feedback" || gotBody.Value != 1 || gotBody.ID != "ts-123-user-feedback" {
		t.Fatalf("body mismatch: %+v", gotBody)
	}
}

func TestClient_CreateScore_RequiresTraceAndName(t *testing.T) {
	c := New("http://example.test", "pk", "sk")
	if err := c.CreateScore(context.Background(), ScoreRequest{Name: "n", Value: 1}); err == nil {
		t.Fatalf("missing trace_id should error")
	}
	if err := c.CreateScore(context.Background(), ScoreRequest{TraceID: "t", Value: 1}); err == nil {
		t.Fatalf("missing name should error")
	}
}

func TestClient_CreateScore_PropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "pk", "sk")
	err := c.CreateScore(context.Background(), ScoreRequest{TraceID: "t", Name: "n", Value: 1})
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("error = %v; want 401 + body excerpt", err)
	}
}
