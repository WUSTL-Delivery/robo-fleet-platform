package fleet

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Credentials are what enrollment hands back and every later hello needs.
//
// A token is minted once by enroll.request and must survive restarts: the server
// never shows it again, and enrolling again mints a NEW identity (a new client
// id). So the client loads from a TokenStore before connecting and enrolls only
// when the store is empty.
type Credentials struct {
	Token    string `json:"token"`
	ClientID string `json:"client_id"`
	FleetID  string `json:"fleet_id"`
}

// TokenStore is where a client keeps its credentials between runs. Load returns
// (nil, nil) when nothing is stored, which makes the client enroll.
type TokenStore interface {
	Load() (*Credentials, error)
	Save(Credentials) error
}

// MemoryTokenStore keeps credentials in memory only: they are lost when the
// process exits. The zero value is an empty store. Safe for concurrent use.
type MemoryTokenStore struct {
	mu    sync.Mutex
	creds *Credentials
}

func (s *MemoryTokenStore) Load() (*Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.creds == nil {
		return nil, nil
	}
	c := *s.creds
	return &c, nil
}

func (s *MemoryTokenStore) Save(c Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = &c
	return nil
}

// FileTokenStore keeps credentials in one JSON file readable only by its owner
// (mode 0600). The format is the one the TypeScript and Python SDKs write, so a
// token file can be handed from one to another.
//
// A missing or corrupt file reads as "nothing stored". Writes go through a temp
// file in the same directory that is synced and then renamed over the old one,
// so a crash never leaves a half-written token. Parent directories are created
// (mode 0700) on save.
type FileTokenStore struct {
	Path string
}

func (s FileTokenStore) Load() (*Credentials, error) {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if json.Unmarshal(raw, &c) != nil || c.Token == "" || c.ClientID == "" || c.FleetID == "" {
		return nil, nil
	}
	return &c, nil
}

func (s FileTokenStore) Save(c Credentials) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// CreateTemp makes the file with mode 0600.
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(s.Path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(raw, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.Path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
