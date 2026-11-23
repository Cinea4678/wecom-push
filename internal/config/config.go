package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultWSURL is the official WeCom AI Bot WebSocket endpoint.
const DefaultWSURL = "wss://openws.work.weixin.qq.com"

// TargetType specifies how WeCom resolves chatid in aibot_send_msg.
type TargetType string

const (
	TargetUser  TargetType = "user"
	TargetGroup TargetType = "group"
	TargetAuto  TargetType = "auto"
)

// ChatType returns the numeric chat_type required by WeCom aibot_send_msg:
// 1 = single (user userid), 2 = group (group chatid), 0 = auto/compat.
func (t TargetType) ChatType() uint32 {
	switch t {
	case TargetUser:
		return 1
	case TargetGroup:
		return 2
	default:
		return 0
	}
}

// Valid reports whether t is a supported target type.
func (t TargetType) Valid() bool {
	return t == TargetUser || t == TargetGroup || t == TargetAuto
}

// Target represents a single WeCom recipient (user or group).
type Target struct {
	Type TargetType `yaml:"type" json:"type"`
	ID   string     `yaml:"id" json:"id"`
	Name string     `yaml:"name,omitempty" json:"name,omitempty"`
}

// Key returns a unique canonical key for deduplication.
func (t Target) Key() string {
	return string(t.Type) + ":" + t.ID
}

// String formats the target in human-readable form.
func (t Target) String() string {
	if t.Name != "" {
		return fmt.Sprintf("%s:%s (%s)", t.Type, t.ID, t.Name)
	}
	return fmt.Sprintf("%s:%s", t.Type, t.ID)
}

// Channel represents a named notification channel with one or more recipients.
type Channel struct {
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Targets     []Target `yaml:"targets" json:"targets"`
}

// BotConfig stores the shared WeCom AI Bot identity.
type BotConfig struct {
	ID     string `yaml:"id" json:"id"`
	Secret string `yaml:"secret" json:"secret"`
	WSURL  string `yaml:"ws_url,omitempty" json:"ws_url,omitempty"`
}

// Config is the root configuration persisted to config.yaml.
type Config struct {
	Bot            BotConfig          `yaml:"bot" json:"bot"`
	DefaultChannel string             `yaml:"default_channel,omitempty" json:"default_channel,omitempty"`
	Channels       map[string]Channel `yaml:"channels,omitempty" json:"channels,omitempty"`
}

var channelNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ValidateChannelName checks that a channel name is clean and CLI-friendly.
func ValidateChannelName(name string) error {
	if name == "" {
		return errors.New("channel name cannot be empty")
	}
	if !channelNameRe.MatchString(name) {
		return fmt.Errorf("invalid channel name %q: must start with alphanumeric and contain only letters, digits, '.', '_', or '-' (max 64 chars)", name)
	}
	return nil
}

// DefaultConfigPath resolves the default config file path using XDG conventions.
func DefaultConfigPath() (string, error) {
	if envPath := strings.TrimSpace(os.Getenv("WECOM_PUSH_CONFIG")); envPath != "" {
		return envPath, nil
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "wecom-push", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "wecom-push", "config.yaml"), nil
}

// ResolvePath returns explicitPath if non-empty, otherwise DefaultConfigPath.
func ResolvePath(explicitPath string) (string, error) {
	if strings.TrimSpace(explicitPath) != "" {
		return explicitPath, nil
	}
	return DefaultConfigPath()
}

// Load reads config from path. If the file does not exist, an empty Config is returned without error.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Channels: make(map[string]Channel),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Channels == nil {
		cfg.Channels = make(map[string]Channel)
	}
	return cfg, nil
}

// Save atomically persists cfg to path with 0600 permissions.
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("nil config")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory %s: %w", dir, err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config %s: %w", path, err)
	}
	cleanup = false
	return nil
}

// ResolvedBot returns BotConfig with environment variable overrides and default WS URL applied.
func (c *Config) ResolvedBot() BotConfig {
	bot := c.Bot
	if v := firstEnv("WECOM_PUSH_BOT_ID", "WECOM_BOT_ID"); v != "" {
		bot.ID = v
	}
	if v := firstEnv("WECOM_PUSH_BOT_SECRET", "WECOM_BOT_SECRET"); v != "" {
		bot.Secret = v
	}
	if v := firstEnv("WECOM_PUSH_WS_URL", "WECOM_WS_URL"); v != "" {
		bot.WSURL = v
	}
	if strings.TrimSpace(bot.WSURL) == "" {
		bot.WSURL = DefaultWSURL
	}
	return bot
}

// ValidateBot ensures Bot ID and Secret are configured.
func (b BotConfig) Validate() error {
	if strings.TrimSpace(b.ID) == "" {
		return errors.New("bot ID is not configured (run `wecom-push init` or set WECOM_PUSH_BOT_ID)")
	}
	if strings.TrimSpace(b.Secret) == "" {
		return errors.New("bot Secret is not configured (run `wecom-push init` or set WECOM_PUSH_BOT_SECRET)")
	}
	return nil
}

// MaskSecret redacts a secret for safe terminal display.
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(not set)"
	}
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:3] + strings.Repeat("*", len(s)-6) + s[len(s)-3:]
}

// ParseTargetSpec parses recipient strings in formats:
//   - "user:<id>" or "user:<id>=<name>"
//   - "group:<id>" or "group:<id>=<name>"
//   - "auto:<id>" or "auto:<id>=<name>"
//   - "<id>" or "<id>=<name>" (uses defaultType if provided)
func ParseTargetSpec(defaultType TargetType, raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Target{}, errors.New("recipient cannot be empty")
	}

	targetType := defaultType
	rest := raw

	if idx := strings.Index(raw, ":"); idx > 0 {
		prefix := strings.ToLower(strings.TrimSpace(raw[:idx]))
		switch TargetType(prefix) {
		case TargetUser, TargetGroup, TargetAuto:
			targetType = TargetType(prefix)
			rest = raw[idx+1:]
		}
	}

	if targetType == "" {
		return Target{}, fmt.Errorf("recipient %q must specify type prefix (user:<id> or group:<id>)", raw)
	}
	if !targetType.Valid() {
		return Target{}, fmt.Errorf("unsupported recipient type %q", targetType)
	}

	var id, name string
	if eq := strings.Index(rest, "="); eq >= 0 {
		id = strings.TrimSpace(rest[:eq])
		name = strings.TrimSpace(rest[eq+1:])
	} else {
		id = strings.TrimSpace(rest)
	}
	if id == "" {
		return Target{}, fmt.Errorf("recipient ID cannot be empty in %q", raw)
	}

	return Target{
		Type: targetType,
		ID:   id,
		Name: name,
	}, nil
}

// AddChannel creates a new channel or appends/replaces targets based on options.
func (c *Config) AddChannel(name string, ch Channel, appendExisting, overwrite bool) error {
	if err := ValidateChannelName(name); err != nil {
		return err
	}
	if len(ch.Targets) == 0 {
		return errors.New("channel must have at least one recipient (--user, --group, or --target)")
	}
	for _, t := range ch.Targets {
		if !t.Type.Valid() || strings.TrimSpace(t.ID) == "" {
			return fmt.Errorf("invalid target: %+v", t)
		}
	}

	if c.Channels == nil {
		c.Channels = make(map[string]Channel)
	}

	existing, exists := c.Channels[name]
	if exists && !appendExisting && !overwrite {
		return fmt.Errorf("channel %q already exists (use --append to add recipients or --force to replace)", name)
	}

	if exists && appendExisting && !overwrite {
		merged := mergeTargets(existing.Targets, ch.Targets)
		desc := existing.Description
		if ch.Description != "" {
			desc = ch.Description
		}
		c.Channels[name] = Channel{
			Description: desc,
			Targets:     merged,
		}
		return nil
	}

	c.Channels[name] = Channel{
		Description: ch.Description,
		Targets:     mergeTargets(nil, ch.Targets),
	}
	return nil
}

// RemoveChannel deletes an entire channel by name.
func (c *Config) RemoveChannel(name string) error {
	if _, ok := c.Channels[name]; !ok {
		return fmt.Errorf("channel %q not found", name)
	}
	delete(c.Channels, name)
	if c.DefaultChannel == name {
		c.DefaultChannel = ""
	}
	return nil
}

// RemoveTargets removes specific targets from a channel. Returns number of removed targets.
func (c *Config) RemoveTargets(name string, toRemove []Target) (int, error) {
	ch, ok := c.Channels[name]
	if !ok {
		return 0, fmt.Errorf("channel %q not found", name)
	}
	removeKeys := make(map[string]bool, len(toRemove))
	for _, t := range toRemove {
		removeKeys[t.Key()] = true
	}
	kept := make([]Target, 0, len(ch.Targets))
	removed := 0
	for _, t := range ch.Targets {
		if removeKeys[t.Key()] {
			removed++
			continue
		}
		kept = append(kept, t)
	}
	if removed == 0 {
		return 0, fmt.Errorf("none of the specified recipients matched in channel %q", name)
	}
	ch.Targets = kept
	c.Channels[name] = ch
	return removed, nil
}

// ChannelNames returns sorted channel names.
func (c *Config) ChannelNames() []string {
	names := make([]string, 0, len(c.Channels))
	for k := range c.Channels {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func mergeTargets(base, extra []Target) []Target {
	out := make([]Target, 0, len(base)+len(extra))
	index := make(map[string]int, len(base)+len(extra))
	for _, t := range base {
		k := t.Key()
		if idx, ok := index[k]; ok {
			if t.Name != "" {
				out[idx].Name = t.Name
			}
			continue
		}
		index[k] = len(out)
		out = append(out, t)
	}
	for _, t := range extra {
		k := t.Key()
		if idx, ok := index[k]; ok {
			if t.Name != "" {
				out[idx].Name = t.Name
			}
			continue
		}
		index[k] = len(out)
		out = append(out, t)
	}
	return out
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
