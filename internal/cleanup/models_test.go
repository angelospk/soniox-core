package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadModelChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cleanup-models")
	if err := os.WriteFile(p, []byte("# header\na, b\tc # tail\n\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ReadModelChain(p), ","); got != "a,b,c,d" {
		t.Fatalf("got %q", got)
	}
	if got := ReadModelChain(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Fatalf("missing file: got %q", got)
	}
}
