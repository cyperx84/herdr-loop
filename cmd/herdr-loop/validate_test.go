package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cyperx84/herdr-loop/internal/engine"
	"github.com/cyperx84/herdr-loop/internal/manifest"
)

// Every example we ship must pass the binary's own validate. Shipping one the
// tool rejects reads as a broken build (F7), and it is the first thing a new
// user runs.
func TestShippedExamplesValidate(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.toml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no examples found — this test is silently vacuous if the path is wrong")
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			m, err := manifest.Parse(data)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			res, err := mapManifest(m)
			if err != nil {
				t.Fatalf("mapManifest: %v", err)
			}
			if _, err := engine.New(res.Config, noopHerdr{}, noopModel{}, discardLogger()); err != nil {
				t.Fatalf("engine.New: %v", err)
			}
			if err := checkKindCapacity(res.Config.Slots, nil); err != nil {
				t.Fatalf("kind capacity: %v", err)
			}
		})
	}
}
