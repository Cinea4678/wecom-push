package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveAndChannels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load missing file failed: %v", err)
	}

	cfg.Bot = BotConfig{
		ID:     "bot_123",
		Secret: "sec_super_secret",
	}

	t1, err := ParseTargetSpec(TargetGroup, "wr12345=生产告警群")
	if err != nil {
		t.Fatalf("ParseTargetSpec group: %v", err)
	}
	t2, err := ParseTargetSpec(TargetUser, "zhangsan=张三")
	if err != nil {
		t.Fatalf("ParseTargetSpec user: %v", err)
	}
	t3, err := ParseTargetSpec("", "user:lisi")
	if err != nil {
		t.Fatalf("ParseTargetSpec prefixed: %v", err)
	}

	if err := cfg.AddChannel("ops", Channel{
		Description: "运维通知",
		Targets:     []Target{t1, t2},
	}, false, false); err != nil {
		t.Fatalf("AddChannel: %v", err)
	}

	// Duplicate without append/force must fail.
	if err := cfg.AddChannel("ops", Channel{Targets: []Target{t3}}, false, false); err == nil {
		t.Fatal("expected duplicate channel error, got nil")
	}

	// Append merges & deduplicates.
	if err := cfg.AddChannel("ops", Channel{Targets: []Target{t2, t3}}, true, false); err != nil {
		t.Fatalf("AddChannel append: %v", err)
	}
	if got := len(cfg.Channels["ops"].Targets); got != 3 {
		t.Fatalf("expected 3 targets after append, got %d", got)
	}

	cfg.DefaultChannel = "ops"
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", perm)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if reloaded.Bot.ID != "bot_123" || reloaded.Bot.Secret != "sec_super_secret" {
		t.Fatalf("unexpected reloaded bot: %+v", reloaded.Bot)
	}
	if reloaded.DefaultChannel != "ops" {
		t.Fatalf("unexpected default channel: %q", reloaded.DefaultChannel)
	}
	if len(reloaded.Channels["ops"].Targets) != 3 {
		t.Fatalf("unexpected targets count: %+v", reloaded.Channels["ops"].Targets)
	}

	removed, err := reloaded.RemoveTargets("ops", []Target{t2})
	if err != nil || removed != 1 {
		t.Fatalf("RemoveTargets: removed=%d err=%v", removed, err)
	}
	if err := reloaded.RemoveChannel("ops"); err != nil {
		t.Fatalf("RemoveChannel: %v", err)
	}
	if reloaded.DefaultChannel != "" {
		t.Fatalf("expected DefaultChannel cleared, got %q", reloaded.DefaultChannel)
	}
}

func TestEnvOverrides(t *testing.T) {
	cfg := &Config{
		Bot: BotConfig{
			ID:     "file_id",
			Secret: "file_secret",
		},
	}
	t.Setenv("WECOM_PUSH_BOT_ID", "env_id")
	t.Setenv("WECOM_BOT_SECRET", "env_secret")
	t.Setenv("WECOM_PUSH_WS_URL", "wss://example.internal/ws")

	bot := cfg.ResolvedBot()
	if bot.ID != "env_id" || bot.Secret != "env_secret" || bot.WSURL != "wss://example.internal/ws" {
		t.Fatalf("unexpected resolved bot: %+v", bot)
	}
}

func TestChatTypeMapping(t *testing.T) {
	if TargetUser.ChatType() != 1 {
		t.Fatalf("expected user chat_type 1, got %d", TargetUser.ChatType())
	}
	if TargetGroup.ChatType() != 2 {
		t.Fatalf("expected group chat_type 2, got %d", TargetGroup.ChatType())
	}
	if TargetAuto.ChatType() != 0 {
		t.Fatalf("expected auto chat_type 0, got %d", TargetAuto.ChatType())
	}
}
