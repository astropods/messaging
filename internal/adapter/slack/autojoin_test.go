package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	slacklib "github.com/slack-go/slack"
)

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
	a, _ := newTestAdapter()
	srv, got, mu := joinRecorder(nil)
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	a.joinObservedChannels(context.Background(), []string{"C1", "C2", "C3"})

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 3 {
		t.Fatalf("expected a join for each channel, got %v", *got)
	}
}

func TestJoinObservedChannels_ContinuesPastAFailure(t *testing.T) {
	a, _ := newTestAdapter()
	srv, got, mu := joinRecorder(map[string]string{
		"C2": "method_not_supported_for_channel_type",
	})
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	a.joinObservedChannels(context.Background(), []string{"C1", "C2", "C3"})

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 3 {
		t.Errorf("a failure must not abort the rest, got %v", *got)
	}
}

func TestJoinObservedChannels_SkipsEmptyEntries(t *testing.T) {
	a, _ := newTestAdapter()
	srv, got, mu := joinRecorder(nil)
	defer srv.Close()
	a.client = slacklib.New("xoxb-fake", slacklib.OptionAPIURL(srv.URL+"/"))

	a.joinObservedChannels(context.Background(), []string{"C1", "", "C2"})

	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 2 {
		t.Errorf("expected empty entries to be skipped, got %v", *got)
	}
}
