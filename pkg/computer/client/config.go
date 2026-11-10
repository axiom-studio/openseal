// Package client is the helloseal bridge that runs on a person's computer.
// It pairs with HelloSeal through a device-code flow (Pair), then long-polls
// for calls and answers them (Serve), running commands and reading, writing
// and listing files only inside the folders the person allowed.
//
// It imports only the standard library and pkg/computer/bridge, so the
// helloseal binary builds with CGO_ENABLED=0 for every platform.
package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultServer is the HelloSeal the bridge pairs with when nothing else is
// named.
const DefaultServer = "https://helloseal.ai"

// ServerEnv names the environment variable that overrides the saved server.
const ServerEnv = "HELLOSEAL_SERVER"

// APIPath is where the bridge API lives under a server URL.
const APIPath = "/rest/v1/helloseal/bridge"

const (
	configFile = "computer.json"
	keyFile    = "key"
)

// ErrNotConnected is returned when this computer has no saved connection.
var ErrNotConnected = errors.New("this computer is not connected; run helloseal connect")

// Config is the saved connection, computer.json.
type Config struct {
	Server     string `json:"server"`
	ComputerID string `json:"computerId"`
	Name       string `json:"name"`
}

// Store is the bridge's config directory: computer.json and the raw ed25519
// private key, both readable only by the person.
type Store struct {
	Dir string
}

// DefaultStore is <UserConfigDir>/helloseal.
func DefaultStore() (Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Store{}, fmt.Errorf("find the config folder: %w", err)
	}
	return Store{Dir: filepath.Join(dir, "helloseal")}, nil
}

// Load reads the saved connection, or ErrNotConnected.
func (s Store) Load() (Config, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotConnected
	}
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("read %s: %w", configFile, err)
	}
	if config.ComputerID == "" || config.Server == "" {
		return Config{}, ErrNotConnected
	}
	return config, nil
}

// Save writes computer.json at mode 0600.
func (s Store) Save(config Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return s.writePrivate(configFile, append(data, '\n'))
}

// Key reads the private key, or ErrNotConnected when there is none.
func (s Store) Key() (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, keyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotConnected
	}
	if err != nil {
		return nil, err
	}
	if len(data) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%s is not an ed25519 private key", filepath.Join(s.Dir, keyFile))
	}
	return ed25519.PrivateKey(data), nil
}

// KeyOrCreate reads the private key, generating and saving one at mode 0600
// when there is none.
func (s Store) KeyOrCreate() (ed25519.PrivateKey, error) {
	key, err := s.Key()
	if !errors.Is(err, ErrNotConnected) {
		return key, err
	}
	_, key, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := s.writePrivate(keyFile, key); err != nil {
		return nil, err
	}
	return key, nil
}

// Delete removes the saved connection and key.
func (s Store) Delete() error {
	for _, name := range []string{configFile, keyFile} {
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writePrivate writes a file only the person can read, replacing it whole.
func (s Store) writePrivate(name string, data []byte) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "."+name+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && !isWindows() {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.Dir, name))
}

// ResolveServer picks the server: the flag, then HELLOSEAL_SERVER, then the
// saved config, then DefaultServer. A trailing slash is dropped.
func ResolveServer(flagValue, saved string) string {
	for _, candidate := range []string{flagValue, os.Getenv(ServerEnv), saved, DefaultServer} {
		if candidate = strings.TrimRight(strings.TrimSpace(candidate), "/"); candidate != "" {
			return candidate
		}
	}
	return DefaultServer
}
