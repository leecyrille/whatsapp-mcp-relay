package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The example config shipped in config/ must load and pass the same checks as a real one.
func TestExampleConfigIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "runner.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}
