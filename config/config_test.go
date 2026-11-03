package config

import (
	"github.com/astropods/messaging/internal/store/files"
	"testing"
)

func TestLoad_SlackConfigJSON_FullConfig(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{"actionable_reactions":["ticket","bug"],"socket_mode":false,"auto_thread":false,"allowed_channel_ids":["ch1","ch2"],"allowed_user_ids":["user1","user2"]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	ac := cfg.Slack.AdapterConfig
	if len(ac.ActionableReactions) != 2 || ac.ActionableReactions[0] != "ticket" || ac.ActionableReactions[1] != "bug" {
		t.Errorf("ActionableReactions = %v, want [ticket bug]", ac.ActionableReactions)
	}
	if ac.SocketMode == nil || *ac.SocketMode != false {
		t.Errorf("SocketMode = %v, want false", ac.SocketMode)
	}
	if ac.AutoThread == nil || *ac.AutoThread != false {
		t.Errorf("AutoThread = %v, want false", ac.AutoThread)
	}
	if len(ac.AllowedChannelIDs) != 2 || ac.AllowedChannelIDs[0] != "ch1" || ac.AllowedChannelIDs[1] != "ch2" {
		t.Errorf("AllowedChannelIDs = %v, want [ch1 ch2]", ac.AllowedChannelIDs)
	}
	if len(ac.AllowedUserIDs) != 2 || ac.AllowedUserIDs[0] != "user1" || ac.AllowedUserIDs[1] != "user2" {
		t.Errorf("AllowedUserIDs = %v, want [user1 user2]", ac.AllowedUserIDs)
	}

	if cfg.Slack.Config.SocketMode != false {
		t.Errorf("adapter.Config.SocketMode = %v, want false", cfg.Slack.Config.SocketMode)
	}
	if cfg.Slack.Config.AutoThread != false {
		t.Errorf("adapter.Config.AutoThread = %v, want false", cfg.Slack.Config.AutoThread)
	}
	if len(cfg.Slack.Config.ActionableReactions) != 2 {
		t.Errorf("adapter.Config.ActionableReactions len = %d, want 2", len(cfg.Slack.Config.ActionableReactions))
	}
	if len(cfg.Slack.Config.AllowedChannelIDs) != 2 {
		t.Errorf("AllowedChannelIDs len = %d, want 2", len(cfg.Slack.Config.AllowedChannelIDs))
	}
	if len(cfg.Slack.Config.AllowedUserIDs) != 2 {
		t.Errorf("AllowedUserIDs len = %d, want 2", len(cfg.Slack.Config.AllowedUserIDs))
	}
}

func TestLoad_SlackConfigJSON_DefaultsWhenAbsent(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	ac := cfg.Slack.AdapterConfig
	if ac.SocketMode == nil || *ac.SocketMode != true {
		t.Errorf("SocketMode default = %v, want true", ac.SocketMode)
	}
	if ac.AutoThread == nil || *ac.AutoThread != true {
		t.Errorf("AutoThread default = %v, want true", ac.AutoThread)
	}
	if len(ac.ActionableReactions) != 0 {
		t.Errorf("ActionableReactions default = %v, want empty", ac.ActionableReactions)
	}
	if len(ac.AllowedChannelIDs) != 0 {
		t.Errorf("AllowedChannelIDs default = %v, want empty", ac.AllowedChannelIDs)
	}
	if len(ac.AllowedUserIDs) != 0 {
		t.Errorf("AllowedUserIDs default = %v, want empty", ac.AllowedUserIDs)
	}

	if cfg.Slack.Config.SocketMode != true {
		t.Errorf("adapter.Config.SocketMode default = %v, want true", cfg.Slack.Config.SocketMode)
	}
	if cfg.Slack.Config.AutoThread != true {
		t.Errorf("adapter.Config.AutoThread default = %v, want true", cfg.Slack.Config.AutoThread)
	}
	if len(cfg.Slack.Config.AllowedChannelIDs) != 0 {
		t.Errorf("AllowedChannelIDs default = %v, want empty", cfg.Slack.Config.AllowedChannelIDs)
	}
	if len(cfg.Slack.Config.AllowedUserIDs) != 0 {
		t.Errorf("AllowedUserIDs default = %v, want empty", cfg.Slack.Config.AllowedUserIDs)
	}
	if len(cfg.Slack.Config.ObserveChannelIDs) != 0 {
		t.Errorf("ObserveChannelIDs default = %v, want empty", cfg.Slack.Config.ObserveChannelIDs)
	}
}

func TestLoad_SlackConfigJSON_ObserveChannelIDs(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{"observe_channel_ids":["C1","C2"]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got := cfg.Slack.Config.ObserveChannelIDs
	if len(got) != 2 || got[0] != "C1" || got[1] != "C2" {
		t.Errorf("ObserveChannelIDs = %v, want [C1 C2]", got)
	}
}

func TestLoad_SlackConfigJSON_MentionUserIDs(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{"mention_user_ids":["U_SOHUM"]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Slack.Config.MentionUserIDs; len(got) != 1 || got[0] != "U_SOHUM" {
		t.Errorf("MentionUserIDs = %v, want [U_SOHUM]: SLACK_CONFIG must reach the adapter config", got)
	}
}

func TestLoad_SlackConfigJSON_PartialJSON(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{"actionable_reactions":["ticket"],"allowed_channel_ids":["ch1"]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	ac := cfg.Slack.AdapterConfig
	if len(ac.ActionableReactions) != 1 || ac.ActionableReactions[0] != "ticket" {
		t.Errorf("ActionableReactions = %v, want [ticket]", ac.ActionableReactions)
	}
	if ac.SocketMode == nil || *ac.SocketMode != true {
		t.Errorf("SocketMode should default to true when omitted from JSON, got %v", ac.SocketMode)
	}
	if ac.AutoThread == nil || *ac.AutoThread != true {
		t.Errorf("AutoThread should default to true when omitted from JSON, got %v", ac.AutoThread)
	}
	if len(ac.AllowedChannelIDs) != 1 || ac.AllowedChannelIDs[0] != "ch1" {
		t.Errorf("AllowedChannelIDs = %v, want [ch1]", ac.AllowedChannelIDs)
	}
	if len(ac.AllowedUserIDs) != 0 {
		t.Errorf("AllowedUserIDs default = %v, want empty", ac.AllowedUserIDs)
	}
}

func TestLoad_SlackConfigJSON_InvalidJSON(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{invalid}`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() should return error for invalid SLACK_CONFIG JSON")
	}
}

func TestLoad_SlackCredentialIsolation(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-secret")
	t.Setenv("SLACK_APP_TOKEN", "xapp-secret")
	t.Setenv("SLACK_CONFIG", `{"actionable_reactions":["ticket"]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.Slack.Credentials.BotToken != "xoxb-secret" {
		t.Errorf("BotToken = %q, want %q", cfg.Slack.Credentials.BotToken, "xoxb-secret")
	}
	if cfg.Slack.Credentials.AppToken != "xapp-secret" {
		t.Errorf("AppToken = %q, want %q", cfg.Slack.Credentials.AppToken, "xapp-secret")
	}
	if cfg.Slack.Config.BotToken != "xoxb-secret" {
		t.Errorf("adapter.Config.BotToken = %q, want %q", cfg.Slack.Config.BotToken, "xoxb-secret")
	}
	if cfg.Slack.Config.AppToken != "xapp-secret" {
		t.Errorf("adapter.Config.AppToken = %q, want %q", cfg.Slack.Config.AppToken, "xapp-secret")
	}
}

func TestLoad_SlackDisabled_NoTokenRequired(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() should not error when Slack is disabled: %v", err)
	}
	if cfg.Slack.Enabled {
		t.Error("Slack.Enabled should be false")
	}
}

func TestLoad_SlackEnabled_MissingBotToken(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() should error when Slack is enabled but SLACK_BOT_TOKEN is missing")
	}
}

func TestLoad_SlackSocketMode_MissingAppToken(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("SLACK_CONFIG", `{"socket_mode":true}`)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() should error when socket_mode=true and SLACK_APP_TOKEN is missing")
	}
}

func TestLoad_SlackActionableReactionsCSV(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_ACTIONABLE_REACTIONS", "ticket, bug, question")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	ar := cfg.Slack.AdapterConfig.ActionableReactions
	if len(ar) != 3 || ar[0] != "ticket" || ar[1] != "bug" || ar[2] != "question" {
		t.Errorf("ActionableReactions = %v, want [ticket bug question]", ar)
	}
}

func TestLoad_SlackConfigJSON_OverridesCSV(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{"actionable_reactions":["from-json"]}`)
	t.Setenv("SLACK_ACTIONABLE_REACTIONS", "from-csv")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	ar := cfg.Slack.AdapterConfig.ActionableReactions
	if len(ar) != 1 || ar[0] != "from-json" {
		t.Errorf("SLACK_CONFIG JSON should take precedence, got %v", ar)
	}
}

func TestLoad_SlackSocketModeDisabled_NoAppTokenRequired(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "")
	t.Setenv("SLACK_CONFIG", `{"socket_mode":false}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() should not require app token when socket_mode=false: %v", err)
	}
	if cfg.Slack.Config.SocketMode != false {
		t.Errorf("SocketMode = %v, want false", cfg.Slack.Config.SocketMode)
	}
}

func TestLoad_MeshJobsParseFromJSON(t *testing.T) {
	t.Setenv("MESH_JOBS", `[{"name":"github.issue.investigate","description":"Investigates a new GitHub issue."},{"name":"notes.summarize"}]`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []MeshJob{
		{Name: "github.issue.investigate", Description: "Investigates a new GitHub issue."},
		{Name: "notes.summarize"},
	}
	if len(cfg.Mesh.Jobs) != len(want) || cfg.Mesh.Jobs[0] != want[0] || cfg.Mesh.Jobs[1] != want[1] {
		t.Errorf("Mesh.Jobs = %+v, want %+v", cfg.Mesh.Jobs, want)
	}
}

func TestLoad_MeshJobsThatAreNotAJSONListAreIgnored(t *testing.T) {
	t.Setenv("MESH_JOBS", "github.issue.investigate")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want the sidecar to start with no declared jobs", err)
	}
	if len(cfg.Mesh.Jobs) != 0 {
		t.Errorf("Mesh.Jobs = %+v, want none from a value that is not JSON", cfg.Mesh.Jobs)
	}
}

func TestLoad_FilesMaxMB(t *testing.T) {
	tests := []struct {
		raw  string
		want int64
	}{
		{"", files.DefaultMaxFileBytes},
		{"250", 250 << 20},
		{"0", files.NoFileLimit},
		{"-1", files.DefaultMaxFileBytes},
		{"lots", files.DefaultMaxFileBytes},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv("FILES_MAX_MB", tt.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v: a bad cap must not stop the sidecar", err)
			}
			if cfg.Files.MaxBytes != tt.want {
				t.Errorf("FILES_MAX_MB=%q: MaxBytes = %d, want %d", tt.raw, cfg.Files.MaxBytes, tt.want)
			}
		})
	}
}
