package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	slacklib "github.com/slack-go/slack"
)

// newJoinTestAdapter is newTestAdapter plus the rate limiter every Web API call
// goes through. Limits are high so the tests are not paced by it.
func newJoinTestAdapter(srv *httptest.Server) *SlackAdapter {
	a, _ := newTestAdapter()
	a.rateLimiter = NewRateLimiter(1000, 1000)
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))
	return a
}

// joinRecorder serves conversations.join, recording the channels asked for and
// failing those named in fail.
func joinRecorder(fail map[string]string) (*httptest.Server, *[]string, *sync.Mutex) {
	var mu sync.Mutex
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("/conversations.join", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ch := r.FormValue("channel")
		mu.Lock()
		got = append(got, ch)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if errStr, bad := fail[ch]; bad {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": errStr})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": map[string]any{"id": ch, "name": "some-channel"},
		})
	})
	mux.HandleFunc("/", jsonOK)
	srv := httptest.NewServer(mux)
	return srv, &got, &mu
}

func TestJoinObservedChannels_JoinsEveryChannel(t *testing.T) {
	srv, got, mu := joinRecorder(nil)
	defer srv.Close()
	a := newJoinTestAdapter(srv)

	a.joinObservedChannels(context.Background(), []string{"C1", "C2", "C3"})

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 3 {
		t.Fatalf("expected a join for each channel, got %v", *got)
	}
}

func TestJoinObservedChannels_ContinuesPastAFailure(t *testing.T) {
	srv, got, mu := joinRecorder(map[string]string{
		"C2": "method_not_supported_for_channel_type",
	})
	defer srv.Close()
	a := newJoinTestAdapter(srv)

	a.joinObservedChannels(context.Background(), []string{"C1", "C2", "C3"})

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 3 {
		t.Errorf("a failure must not abort the rest, got %v", *got)
	}
}

func TestJoinObservedChannels_SkipsEmptyEntries(t *testing.T) {
	srv, got, mu := joinRecorder(nil)
	defer srv.Close()
	a := newJoinTestAdapter(srv)

	a.joinObservedChannels(context.Background(), []string{"C1", "", "C2"})

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Errorf("expected empty entries to be skipped, got %v", *got)
	}
}

// The limiter gates every Web API call in this adapter; conversations.join is
// Tier-3, so a long list would otherwise burst past it.
func TestJoinObservedChannels_WaitsOnTheRateLimiter(t *testing.T) {
	srv, got, mu := joinRecorder(nil)
	defer srv.Close()
	a := newJoinTestAdapter(srv)
	a.rateLimiter = NewRateLimiter(0, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	a.joinObservedChannels(ctx, []string{"C1", "C2"})

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Errorf("no join should be attempted while the limiter cannot proceed, got %v", *got)
	}
}
