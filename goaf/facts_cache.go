package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// cachedFacts is one host's facts with a collection timestamp.
type cachedFacts struct {
	Facts      Facts `json:"facts"`
	GatheredAt int64 `json:"gathered_at"`
}

// factsCachePath returns ~/.goaf/facts.json ("" when home is unknown).
func factsCachePath() string {
	home := homeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".goaf", "facts.json")
}

// loadFactsCache reads the whole facts cache (empty map on any problem;
// a corrupt cache must never break a run).
func loadFactsCache() map[string]cachedFacts {
	out := map[string]cachedFacts{}
	path := factsCachePath()
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// saveFactsCache persists the cache (best effort).
func saveFactsCache(cache map[string]cachedFacts) {
	path := factsCachePath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// clearFactsCache drops all cached facts.
func clearFactsCache() {
	path := factsCachePath()
	if path == "" {
		return
	}
	_ = os.Remove(path)
}
