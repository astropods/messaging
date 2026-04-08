package config

import (
	"testing"
)

func TestLoad_SlackConfigJSON_FullConfig(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{"actionable_reactions":["ticket","bug"],"socket_mode":false,"auto_thread":false,"allowed_channel_ids":["ch1","ch2"],"admin_user_ids":["user1","user2"]}`)

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
	if len(ac.AdminUserIDs) != 2 || ac.AdminUserIDs[0] != "user1" || ac.AdminUserIDs[1] != "user2" {
		t.Errorf("AdminUserIDs = %v, want [user1 user2]", ac.AdminUserIDs)
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
	if len(cfg.Slack.Config.AdminUserIDs) != 2 {
		t.Errorf("AdminUserIDs len = %d, want 2", len(cfg.Slack.Config.AdminUserIDs))
	}
}

func TestLoad_SlackConfigJSON_DeprecatedAllowedUserIDs(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	// Use deprecated allowed_user_ids key — should be accepted and mapped to AdminUserIDs
	t.Setenv("SLACK_CONFIG", `{"allowed_user_ids":["user1","user2"]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Slack.Config.AdminUserIDs) != 2 {
		t.Errorf("AdminUserIDs len = %d, want 2 (via deprecated allowed_user_ids)", len(cfg.Slack.Config.AdminUserIDs))
	}
}

func TestLoad_SlackConfigJSON_DeprecatedAllowedUserIDsEnvVar(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	// Use deprecated SLACK_ALLOWED_USER_IDS env var — should be accepted and mapped to AdminUserIDs
	t.Setenv("SLACK_ALLOWED_USER_IDS", "user1,user2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Slack.Config.AdminUserIDs) != 2 {
		t.Errorf("AdminUserIDs len = %d, want 2 (via deprecated SLACK_ALLOWED_USER_IDS)", len(cfg.Slack.Config.AdminUserIDs))
	}
}

func TestLoad_SlackConfigJSON_AdminUserIDsTakePrecedenceOverDeprecated(t *testing.T) {
	t.Setenv("SLACK_ENABLED", "true")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_CONFIG", `{"admin_user_ids":["admin1"],"allowed_user_ids":["old1","old2"]}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(cfg.Slack.Config.AdminUserIDs) != 1 || cfg.Slack.Config.AdminUserIDs[0] != "admin1" {
		t.Errorf("AdminUserIDs = %v, want [admin1] (admin_user_ids should take precedence)", cfg.Slack.Config.AdminUserIDs)
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
	if len(ac.AdminUserIDs) != 0 {
		t.Errorf("AdminUserIDs default = %v, want empty", ac.AdminUserIDs)
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
	if len(cfg.Slack.Config.AdminUserIDs) != 0 {
		t.Errorf("AdminUserIDs default = %v, want empty", cfg.Slack.Config.AdminUserIDs)
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
	if len(ac.AdminUserIDs) != 0 {
		t.Errorf("AdminUserIDs default = %v, want empty", ac.AdminUserIDs)
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
