package slack

import (
	"strings"
	"testing"
)

func TestFormatSlackMetaLine_IncludesThreadWhenDifferent(t *testing.T) {
	line := formatSlackMetaLine("C1", "100.1", "100.0", "https://example.com/p")
	if !strings.HasPrefix(line, "[slack_meta] ") {
		t.Fatalf("expected prefix, got %q", line)
	}
	for _, want := range []string{`"channel_id":"C1"`, `"message_ts":"100.1"`, `"thread_ts":"100.0"`, `"permalink":"https://example.com/p"`} {
		if !strings.Contains(line, want) {
			t.Errorf("meta line %q missing %q", line, want)
		}
	}
}

func TestFormatSlackMetaLine_OmitsThreadWhenSameAsMessage(t *testing.T) {
	line := formatSlackMetaLine("C1", "100.1", "100.1", "")
	if strings.Contains(line, "thread_ts") {
		t.Errorf("expected no thread_ts when root equals message, got %q", line)
	}
}

func TestResolveSlackPermalink(t *testing.T) {
	got := resolveSlackPermalink("https://acme.slack.com/", "C99", "1234567890.123456")
	want := "https://acme.slack.com/archives/C99/p1234567890123456"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestPrependSlackMeta(t *testing.T) {
	got := prependSlackMeta("hello", "[slack_meta] {}")
	if got != "[slack_meta] {}\nhello" {
		t.Errorf("got %q", got)
	}
}
