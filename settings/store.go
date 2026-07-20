package settings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sausheong/octopus/config"
)

type Store struct {
	path string
}

func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) Path() string { return s.path }

func (s *Store) Load() (Document, []byte, bool, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		doc := defaultDocument()
		cfg, cfgErr := doc.config()
		if cfgErr != nil {
			return Document{}, nil, false, cfgErr
		}
		raw, marshalErr := config.Marshal(cfg)
		return doc, raw, false, marshalErr
	}
	if err != nil {
		return Document{}, nil, false, fmt.Errorf("read settings: %w", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return defaultDocument(), data, true, err
	}
	return documentFromConfig(cfg), data, true, nil
}

func (s *Store) SaveDocument(doc Document) ([]byte, error) {
	cfg, err := doc.config()
	if err != nil {
		return nil, err
	}
	data, err := config.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return data, s.atomicWrite(data)
}

func (s *Store) SaveYAML(data []byte) (Document, error) {
	cfg, err := config.Parse(data)
	if err != nil {
		return Document{}, err
	}
	if err := s.atomicWrite(data); err != nil {
		return Document{}, err
	}
	return documentFromConfig(cfg), nil
}

func (s *Store) atomicWrite(data []byte) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure settings directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create settings file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure settings file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write settings: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync settings: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close settings: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}
