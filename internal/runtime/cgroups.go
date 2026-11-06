package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const cgroupRoot = "/sys/fs/cgroup"

// defaultCPUPeriod is the cpu.max period the kernel uses when none is given.
const defaultCPUPeriod = 100000

// cgroupPlan is the cgroup a container goes into and what to write there.
type cgroupPlan struct {
	Dir         string
	Controllers []string
	Files       map[string]string
}

// planCgroup decides whether the container needs a cgroup. Without
// linux.cgroupsPath and without limits it stays in the caller's cgroup.
func planCgroup(l *Linux, id string, selfCgroup []byte) (*cgroupPlan, error) {
	files, controllers := cgroupFiles(l.Resources)
	path := l.CgroupsPath
	if path == "" {
		if len(files) == 0 {
			return nil, nil
		}
		path = "anden-runtime-" + id
	}
	dir, err := resolveCgroupPath(path, selfCgroup)
	if err != nil {
		return nil, err
	}
	return &cgroupPlan{Dir: dir, Controllers: controllers, Files: files}, nil
}

// resolveCgroupPath turns linux.cgroupsPath into a directory. An absolute
// path is relative to the cgroup2 mount; a relative one hangs from the
// cgroup of the calling process, read from /proc/self/cgroup.
func resolveCgroupPath(path string, selfCgroup []byte) (string, error) {
	if strings.Contains(path, ":") {
		return "", fmt.Errorf("linux.cgroupsPath %q: the systemd slice:prefix:name form is not supported", path)
	}
	if filepath.IsAbs(path) {
		return filepath.Join(cgroupRoot, filepath.Clean(path)), nil
	}
	var current string
	for _, line := range strings.Split(string(selfCgroup), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			current = rest
			break
		}
	}
	if current == "" {
		return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
	}
	base := filepath.Clean("/" + current)
	joined := filepath.Join(base, path)
	if joined == base || (base != "/" && !strings.HasPrefix(joined, base+"/")) {
		return "", fmt.Errorf("linux.cgroupsPath %q must name a cgroup below the current one", path)
	}
	return filepath.Join(cgroupRoot, joined), nil
}

// cgroupFiles maps linux.resources to cgroup v2 interface files.
func cgroupFiles(r *Resources) (map[string]string, []string) {
	files := map[string]string{}
	var controllers []string
	if r == nil {
		return files, nil
	}
	if r.Memory != nil && r.Memory.Limit != nil {
		v := "max"
		if *r.Memory.Limit >= 0 {
			v = strconv.FormatInt(*r.Memory.Limit, 10)
		}
		files["memory.max"] = v
		controllers = append(controllers, "memory")
	}
	if r.CPU != nil && (r.CPU.Quota != nil || r.CPU.Period != nil) {
		quota := "max"
		if r.CPU.Quota != nil && *r.CPU.Quota > 0 {
			quota = strconv.FormatInt(*r.CPU.Quota, 10)
		}
		period := uint64(defaultCPUPeriod)
		if r.CPU.Period != nil {
			period = *r.CPU.Period
		}
		files["cpu.max"] = quota + " " + strconv.FormatUint(period, 10)
		controllers = append(controllers, "cpu")
	}
	if r.Pids != nil && r.Pids.Limit != nil {
		v := "max"
		if *r.Pids.Limit > 0 {
			v = strconv.FormatInt(*r.Pids.Limit, 10)
		}
		files["pids.max"] = v
		controllers = append(controllers, "pids")
	}
	return files, controllers
}

// setupCgroup creates the cgroup, enables its controllers along the path
// and writes the limits. The container cgroup stays a leaf: controllers
// are enabled only in its ancestors, and an ancestor that still holds
// processes cannot enable them (no internal processes rule), which is
// reported instead of moving someone else's processes.
func setupCgroup(p *cgroupPlan) (created bool, err error) {
	if _, err := os.Stat(p.Dir); errors.Is(err, os.ErrNotExist) {
		created = true
	}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return false, fmt.Errorf("cgroup %s: %w", p.Dir, err)
	}
	if len(p.Controllers) > 0 {
		if err := enableControllers(p.Dir, p.Controllers); err != nil {
			if created {
				os.Remove(p.Dir)
			}
			return false, err
		}
	}
	for name, value := range p.Files {
		if err := os.WriteFile(filepath.Join(p.Dir, name), []byte(value), 0); err != nil {
			if created {
				os.Remove(p.Dir)
			}
			return false, fmt.Errorf("cgroup %s: write %s: %w", p.Dir, name, err)
		}
	}
	return created, nil
}

func enableControllers(dir string, controllers []string) error {
	rel, err := filepath.Rel(cgroupRoot, filepath.Dir(dir))
	if err != nil {
		return err
	}
	ancestors := []string{cgroupRoot}
	if rel != "." {
		cur := cgroupRoot
		for _, part := range strings.Split(rel, "/") {
			cur = filepath.Join(cur, part)
			ancestors = append(ancestors, cur)
		}
	}
	for _, a := range ancestors {
		data, err := os.ReadFile(filepath.Join(a, "cgroup.subtree_control"))
		if err != nil {
			return fmt.Errorf("cgroup %s: %w", a, err)
		}
		enabled := strings.Fields(string(data))
		var missing []string
		for _, c := range controllers {
			if !slices.Contains(enabled, c) {
				missing = append(missing, "+"+c)
			}
		}
		if len(missing) == 0 {
			continue
		}
		err = os.WriteFile(filepath.Join(a, "cgroup.subtree_control"), []byte(strings.Join(missing, " ")), 0)
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("cgroup %s holds processes, so it cannot enable %s for its children (no internal processes rule); move them to a leaf cgroup first", a, strings.Join(missing, " "))
		}
		if err != nil {
			return fmt.Errorf("cgroup %s: enable %s: %w", a, strings.Join(missing, " "), err)
		}
	}
	return nil
}

// removeCgroup kills whatever is left in a cgroup the runtime created and
// removes it.
func removeCgroup(dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cgroup %s: kill: %w", dir, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Remove(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return fmt.Errorf("cgroup %s: remove: %w", dir, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
