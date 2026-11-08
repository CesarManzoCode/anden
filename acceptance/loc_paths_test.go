// Package acceptance holds the acceptance scripts; this test only checks the paths C29 counts.
package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocPathsExist checks that loc_paths.txt declares, as D27 says, existing directories of both classes and nothing of gen/.
func TestLocPathsExist(t *testing.T) {
	data, err := os.ReadFile("loc_paths.txt")
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]int{}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Errorf("line %d: %q is not `<class> <path>`", i+1, line)
			continue
		}
		class, path := fields[0], strings.Trim(fields[1], "/")
		if class != "product" && class != "support" {
			t.Errorf("line %d: unknown class %q", i+1, class)
		}
		if path == "gen" || strings.HasPrefix(path, "gen/") {
			t.Errorf("line %d: gen/ is counted in no class", i+1)
		}
		info, err := os.Stat(filepath.Join("..", path))
		if err != nil || !info.IsDir() {
			t.Errorf("line %d: %s is not a directory of the repository", i+1, path)
		}
		classes[class]++
	}
	if classes["product"] == 0 || classes["support"] == 0 {
		t.Errorf("classes %v, want paths of product and support", classes)
	}
}
