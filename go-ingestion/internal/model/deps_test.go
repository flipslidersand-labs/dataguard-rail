package model_test

import (
	"os/exec"
	"strings"
	"testing"
)

// store / server / pipeline は exec アダプタ層 engine に依存してはならない (issue #187)。
func TestNoEngineDependency(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go コマンドが見つからない")
	}
	const engine = "github.com/flipslidersand-labs/dataguard-rail/internal/engine"
	for _, pkg := range []string{"store", "server", "pipeline"} {
		out, err := exec.Command("go", "list", "-deps", "../"+pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", pkg, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			if dep == engine {
				t.Errorf("internal/%s が internal/engine に依存している", pkg)
			}
		}
	}
}
