package pbp

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedExampleMatches(t *testing.T) {
	disk, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(ExampleConfig) == 0 || !bytes.Equal(ExampleConfig, disk) {
		t.Fatal("embedded example differs from config.example.yaml")
	}
}
