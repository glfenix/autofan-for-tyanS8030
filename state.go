package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

func logf(format string, a ...any) {
	log.Printf(format, a...)
}

// Verbose enables the per-cycle "nothing changed" lines (-v).
var Verbose bool

func vlogf(format string, a ...any) {
	if Verbose {
		log.Printf(format, a...)
	}
}

// StateFile remembers the duty of every group, so a restart of the daemon (or
// a crash) can be reasoned about, and so a group without data does not drop
// the fans to its minimum.
type StateFile struct {
	Updated    time.Time      `json:"updated"`
	Duties     map[string]int `json:"duties"`
	LegacyDuty int            `json:"duty,omitempty"` // старый одногрупповой формат
}

func LoadState(path string) (*StateFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s StateFile
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func SaveState(path string, groups []*GroupState) error {
	d := StateFile{Updated: time.Now(), Duties: map[string]int{}}
	for _, g := range groups {
		d.Duties[g.cfg.Name] = g.lastDuty
	}
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
