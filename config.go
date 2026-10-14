package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const pluginID = "remote-compaction"
const version = "0.1.1"
const maxPayloadBytes = 64 << 20
const maxSummaryBytes = 256 << 10

type config struct {
	KeyFile         string   `yaml:"key_file"`
	Models          []string `yaml:"models"`
	MaxOutputTokens int      `yaml:"max_output_tokens"`
}

type runtimeConfig struct {
	config
	aead   cipher.AEAD
	models map[string]bool
}
type runtimeState struct {
	sync.Mutex
	cond     *sync.Cond
	config   *runtimeConfig
	active   int
	draining bool
}

var state = func() *runtimeState { s := &runtimeState{}; s.cond = sync.NewCond(&s.Mutex); return s }()

func newRuntime(c config) (*runtimeConfig, error) {
	if c.KeyFile == "" || !filepath.IsAbs(c.KeyFile) {
		return nil, fmt.Errorf("key_file must be an absolute path to a private 32-byte key")
	}
	info, err := os.Lstat(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect capsule key file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 32 {
		return nil, fmt.Errorf("capsule key must be a regular 32-byte file with mode 0600 or stricter")
	}
	key, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read capsule key file")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("invalid capsule key")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if c.MaxOutputTokens == 0 {
		c.MaxOutputTokens = 4096
	}
	if c.MaxOutputTokens < 256 || c.MaxOutputTokens > 16384 {
		return nil, fmt.Errorf("max_output_tokens must be between 256 and 16384")
	}
	if len(c.Models) == 0 {
		return nil, fmt.Errorf("models must be an explicit nonempty allowlist")
	}
	r := &runtimeConfig{config: c, aead: aead, models: make(map[string]bool)}
	for _, model := range c.Models {
		m := canonical(model)
		if m == "" || strings.ContainsAny(m, "*?/") || strings.HasPrefix(m, "gpt-") {
			return nil, fmt.Errorf("models must be exact non-GPT identifiers")
		}
		r.models[m] = true
	}
	return r, nil
}

func canonical(model string) string { return strings.ToLower(strings.TrimSpace(model)) }
func (s *runtimeState) acquire() (*runtimeConfig, func(), error) {
	s.Lock()
	defer s.Unlock()
	if s.draining || s.config == nil {
		return nil, nil, &statusError{503, "remote compaction plugin is draining or unconfigured"}
	}
	s.active++
	return s.config, func() { s.Lock(); s.active--; s.cond.Broadcast(); s.Unlock() }, nil
}
func (s *runtimeState) quiesce() {
	s.Lock()
	defer s.Unlock()
	s.draining = true
	for s.active > 0 {
		s.cond.Wait()
	}
}
func (s *runtimeState) configure(raw []byte) error {
	var c config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("invalid plugin configuration")
	}
	r, err := newRuntime(c)
	if err != nil {
		return err
	}
	s.Lock()
	defer s.Unlock()
	// CPA may re-send configuration when credentials or model lists refresh.
	// Each in-flight compaction holds its own immutable runtime snapshot, so
	// swapping the config must not unregister the plugin or drop its hooks.
	s.config, s.draining = r, false
	return nil
}

func generateKey(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("key path must be absolute")
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create key (existing keys are never overwritten): %w", err)
	}
	_, errWrite := f.Write(key)
	errClose := f.Close()
	if errWrite != nil {
		return errWrite
	}
	return errClose
}
