package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const notDynamic = "not a dynamic executable"

// lddLibraries returns the absolute paths ldd reports for bin. A static binary has none.
func lddLibraries(bin string) ([]string, error) {
	cmd := exec.Command("ldd", bin)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), notDynamic) {
		return nil, fmt.Errorf("ldd %s: %w: %s", bin, err, out)
	}
	return parseLdd(string(out))
}

// parseLdd keeps both sides of a line like "/lib64/ld.so => /usr/lib64/ld.so":
// the left one is the interpreter path written in the binary, the right one
// where it resolved.
func parseLdd(output string) ([]string, error) {
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, notDynamic) || strings.Contains(line, "statically linked") {
			return nil, nil
		}
		if strings.HasSuffix(line, "not found") {
			return nil, fmt.Errorf("ldd: missing library: %s", line)
		}
		fields := strings.Fields(line)
		for i, field := range fields {
			if !strings.HasPrefix(field, "/") {
				continue
			}
			if i == 0 || fields[i-1] == "=>" {
				seen[field] = true
			}
		}
	}
	libs := make([]string, 0, len(seen))
	for lib := range seen {
		libs = append(libs, lib)
	}
	sort.Strings(libs)
	return libs, nil
}

// packImage puts bin and libs, at their absolute paths, in one layer, plus the
// directories a container needs as mount points.
func packImage(bin string, libs, cmd, env []string) (image, error) {
	dest := "/usr/bin/" + filepath.Base(bin)
	sources := map[string]string{dest: bin}
	for _, lib := range libs {
		sources[lib] = lib
	}
	dirs := map[string]int64{"dev": 0o755, "proc": 0o755, "sys": 0o755, "tmp": 0o1777}
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
		for parent := filepath.Dir(path); parent != "/"; parent = filepath.Dir(parent) {
			name := strings.TrimPrefix(parent, "/")
			if _, ok := dirs[name]; !ok {
				dirs[name] = 0o755
			}
		}
	}
	sort.Strings(paths)
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]entry, 0, len(names)+len(paths))
	for _, name := range names {
		entries = append(entries, dirEntry(name, dirs[name]))
	}
	for _, path := range paths {
		info, err := os.Stat(sources[path])
		if err != nil {
			return image{}, err
		}
		content, err := os.ReadFile(sources[path])
		if err != nil {
			return image{}, err
		}
		entries = append(entries, fileEntry(strings.TrimPrefix(path, "/"), int64(info.Mode().Perm()), content))
	}
	if len(cmd) == 0 {
		cmd = []string{dest}
	}
	return image{layers: []layerSpec{{entries: entries, compress: true}}, cmd: cmd, env: env}, nil
}
