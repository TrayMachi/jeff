package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
	"github.com/local/jeff/internal/contexts"
)

type Config struct {
	TelegramBotToken     string
	TelegramAllowedChats map[int64]bool
	TelegramForumChatID  int64
	OpencodeBaseURL      string
	OpencodeUsername     string
	OpencodePassword     string
	ConfigFilePath       string
	DataPath             string
	root                 string
}

func Load() (*Config, error) {
	root, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	_ = godotenv.Load(filepath.Join(root, ".env"))
	cfg := &Config{
		TelegramBotToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		OpencodeBaseURL:  os.Getenv("OPENCODE_BASE_URL"),
		OpencodeUsername: os.Getenv("OPENCODE_SERVER_USERNAME"),
		OpencodePassword: os.Getenv("OPENCODE_SERVER_PASSWORD"),
		ConfigFilePath:   os.Getenv("CONFIG_PATH"),
		DataPath:         os.Getenv("DATA_PATH"),
		root:             root,
	}
	if cfg.OpencodeBaseURL == "" {
		cfg.OpencodeBaseURL = "http://127.0.0.1:4096"
	}
	if cfg.OpencodeUsername == "" {
		cfg.OpencodeUsername = "opencode"
	}
	if cfg.ConfigFilePath == "" {
		cfg.ConfigFilePath = filepath.Join(root, "config.yaml")
	}
	if cfg.DataPath == "" {
		cfg.DataPath = filepath.Join(root, "data", "jeff.sqlite")
	}
	allowed, err := parseChatIDs(os.Getenv("TELEGRAM_ALLOWED_CHAT_IDS"))
	if err != nil {
		return nil, err
	}
	cfg.TelegramAllowedChats = allowed
	if raw := strings.TrimSpace(os.Getenv("TELEGRAM_FORUM_CHAT_ID")); raw != "" {
		cfg.TelegramForumChatID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid Telegram forum chat id %q: %w", raw, err)
		}
	}
	if strings.TrimSpace(cfg.TelegramBotToken) == "" {
		return nil, errors.New("TELEGRAM_BOT_TOKEN must not be empty")
	}
	return cfg, nil
}

func parseChatIDs(value string) (map[int64]bool, error) {
	out := map[int64]bool{}
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid Telegram chat id %q: %w", raw, err)
		}
		out[id] = true
	}
	return out, nil
}

func (c *Config) ConfigPath() string { return c.ConfigFilePath }
func (c *Config) DBPath() string     { return c.DataPath }
func (c *Config) OpencodePinnedVersion() (string, error) {
	b, err := os.ReadFile(filepath.Join(c.root, "deploy", "opencode.version"))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("opencode version pin is empty")
	}
	return v, nil
}

func LoadProjects(path string) (*contexts.FileConfig, error) { return contexts.Load(path) }
