package acceptance

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const designSections = "# Diseño\n\n## Decisiones\n\nx\n\n## Alternativas descartadas\n\nx\n\n## Deuda técnica conocida\n\nx\n"

// runC30 runs c30_design.py on a temporary DESIGN.md with content and returns its exit code and output.
func runC30(t *testing.T, content string) (int, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	design := filepath.Join(t.TempDir(), "DESIGN.md")
	if err := os.WriteFile(design, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "c30_design.py", design).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exit):
		return exit.ExitCode(), string(out)
	}
	t.Fatalf("c30_design.py: %v: %s", err, out)
	return 0, ""
}

// TestC30DetectsMissingSection checks that C30 fails on a design without `## Lo que no se demostró`, also when the
// heading only appears inside a code block, and passes once the section is there.
func TestC30DetectsMissingSection(t *testing.T) {
	if rc, out := runC30(t, designSections); rc != 1 || !strings.Contains(out, "missing") {
		t.Errorf("without the section: exit %d, output %q; want exit 1 naming the missing section", rc, out)
	}
	fenced := designSections + "\n```\n## Lo que no se demostró\n```\n"
	if rc, out := runC30(t, fenced); rc != 1 {
		t.Errorf("with the heading only in a code block: exit %d, output %q; want 1", rc, out)
	}
	complete := designSections + "\n## Lo que no se demostró\n\nx\n"
	if rc, out := runC30(t, complete); rc != 0 {
		t.Errorf("with the four sections: exit %d, output %q; want 0", rc, out)
	}
}
