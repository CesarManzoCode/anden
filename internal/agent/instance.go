package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	andenv1 "github.com/CesarManzoCode/anden/gen/anden/v1"
	"github.com/CesarManzoCode/anden/internal/image"
)

const (
	readyPoll     = 10 * time.Millisecond
	probeLimit    = time.Second
	stopGrace     = 2 * time.Second
	killTimeout   = 5 * time.Second
	routeRelaunch = "relaunch"
)

// paths are the places an instance occupies on the node (contract, section 3).
type paths struct {
	dir, upper, work, merged, bundle string
	netNS, utsNS, ipcNS              string
	cgroup                           string
}

func instancePaths(state, cgroup, service string, generation uint64, id string) paths {
	dir := filepath.Join(state, "instances", id)
	return paths{
		dir:    dir,
		upper:  filepath.Join(dir, "upper"),
		work:   filepath.Join(dir, "work"),
		merged: filepath.Join(dir, "merged"),
		bundle: filepath.Join(dir, "bundle"),
		netNS:  filepath.Join(dir, "net.ns"),
		utsNS:  filepath.Join(dir, "uts.ns"),
		ipcNS:  filepath.Join(dir, "ipc.ns"),
		cgroup: filepath.Join(cgroup, "svc-"+service+"-"+strconv.FormatUint(generation, 10)),
	}
}

// instance is one instance of the node. a.mu guards state and stopping.
type instance struct {
	id, service         string
	version, generation uint64
	addr, route         string
	seccomp             bool
	paths               paths

	state    andenv1.InstanceState
	stopping bool

	// cancel ends a start in progress; started closes when it is over.
	cancel  context.CancelFunc
	ctx     context.Context
	started chan struct{}
}

func (a *Agent) newInstance(ctx context.Context, as *andenv1.Assignment) *instance {
	spec := as.GetSpec()
	ictx, cancel := context.WithCancel(ctx)
	return &instance{
		id:         as.GetInstanceId(),
		service:    spec.GetName(),
		version:    as.GetVersion(),
		generation: as.GetGeneration(),
		addr:       as.GetAddr(),
		route:      routeRelaunch,
		seccomp:    seccompEnabled(spec),
		paths:      instancePaths(a.cfg.State, a.cfg.Cgroup, spec.GetName(), as.GetGeneration(), as.GetInstanceId()),
		state:      andenv1.InstanceState_INSTANCE_STATE_STARTING,
		cancel:     cancel,
		ctx:        ictx,
		started:    make(chan struct{}),
	}
}

// start brings an instance to RUNNING, or to FAILED with no process left.
// What it created stays for stop to remove.
func (a *Agent) start(in *instance, as *andenv1.Assignment) {
	defer close(in.started)
	err := a.launch(in, as)
	if err == nil {
		err = a.awaitReady(in, as.GetSpec())
	}
	if err == nil {
		a.setState(in, andenv1.InstanceState_INSTANCE_STATE_RUNNING)
		log.Printf("instance %s: running at %s", in.id, in.addr)
		return
	}
	if in.ctx.Err() != nil {
		return
	}
	log.Printf("instance %s: %v", in.id, err)
	if err := killCgroup(in.paths.cgroup); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("instance %s: %v", in.id, err)
	}
	a.setState(in, andenv1.InstanceState_INSTANCE_STATE_FAILED)
}

// launch prepares the rootfs, cgroup and network and has the runtime start the
// process.
func (a *Agent) launch(in *instance, as *andenv1.Assignment) error {
	p, spec := in.paths, as.GetSpec()
	for _, d := range []string{p.upper, p.work, p.merged, p.bundle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	img, err := a.mountImage(in.ctx, spec.GetImageDigest(), p)
	if err != nil {
		return fmt.Errorf("image %s: %w", spec.GetImageDigest(), err)
	}
	stdio, err := prepareRootfs(p.merged, a.dns)
	if err != nil {
		return err
	}
	defer stdio.Close()
	if err := setupCgroup(p.cgroup); err != nil {
		return err
	}
	if err := a.setupNetwork(in); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	config, err := buildConfig(configInput{spec: spec, run: img.Run, rootfs: p.merged, netNS: p.netNS, utsNS: p.utsNS, ipcNS: p.ipcNS, cgroup: p.cgroup})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(p.bundle, "config.json"), raw, 0o644); err != nil {
		return err
	}
	if err := in.ctx.Err(); err != nil {
		return err
	}
	// The container inherits these, and so does the runtime's own message if
	// create fails.
	create := exec.Command(a.cfg.Runtime, a.runtimeArgs("create", "--bundle", p.bundle, in.id)...)
	create.Stdout, create.Stderr = stdio, stdio
	if err := create.Run(); err != nil {
		return fmt.Errorf("anden-runtime create: %w; see /.anden/stdio.log in %s", err, p.merged)
	}
	if out, err := exec.Command(a.cfg.Runtime, a.runtimeArgs("start", in.id)...).CombinedOutput(); err != nil {
		return fmt.Errorf("anden-runtime start: %w: %s", err, out)
	}
	return nil
}

func (a *Agent) runtimeArgs(args ...string) []string {
	return append([]string{"--root", filepath.Join(a.cfg.State, "runtime")}, args...)
}

// mountImage brings whatever the node lacks of an image from the blob server
// and mounts it as the rootfs of an instance. The store verifies each blob
// against its digest.
func (a *Agent) mountImage(ctx context.Context, digest string, p paths) (image.Image, error) {
	d, err := image.ParseDigest(digest)
	if err != nil {
		return image.Image{}, err
	}
	a.mu.Lock()
	base := a.blobsURL
	a.mu.Unlock()
	a.imageMu.Lock()
	defer a.imageMu.Unlock()
	img, err := a.store.Fetch(d, func(blob image.Digest) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/blobs/sha256/"+blob.Hex(), nil)
		if err != nil {
			return nil, err
		}
		resp, err := a.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("blob %s: %s", blob, resp.Status)
		}
		return resp.Body, nil
	})
	if err != nil {
		return image.Image{}, err
	}
	return img, a.store.Mount(d, p.merged, p.upper, p.work)
}

// makeDir makes dir inside the rootfs. A symlink from the image is refused:
// the agent runs outside the container and must not follow it.
func makeDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.Mkdir(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s in the image is not a directory", dir)
	}
	return nil
}

// createFile replaces path with a new regular file, never through a symlink.
func createFile(path string, flags int) (*os.File, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|flags, 0o644)
}

// prepareRootfs writes /etc/resolv.conf and returns the open /.anden/stdio.log.
func prepareRootfs(merged, dns string) (*os.File, error) {
	etc := filepath.Join(merged, "etc")
	if err := makeDir(etc); err != nil {
		return nil, err
	}
	resolv, err := createFile(filepath.Join(etc, "resolv.conf"), os.O_WRONLY)
	if err != nil {
		return nil, err
	}
	_, err = resolv.WriteString("nameserver " + dns + "\n")
	if cerr := resolv.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(merged, ".anden")
	if err := makeDir(dir); err != nil {
		return nil, err
	}
	return createFile(filepath.Join(dir, "stdio.log"), os.O_WRONLY|os.O_APPEND)
}

// setupCgroup creates the cgroup of an instance. The runtime writes its limits;
// swap is written here because the runtime does not manage it, and with swap a
// process over memory.max is slowed down instead of killed.
func setupCgroup(dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	err := os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte("0"), 0o644)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory.swap.max: %w", err)
	}
	return nil
}

func (a *Agent) setupNetwork(in *instance) error {
	p := in.paths
	for _, f := range []string{p.netNS, p.utsNS, p.ipcNS} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			return err
		}
	}
	if out, err := exec.Command("unshare", namespaceArgs(p.netNS, p.utsNS, p.ipcNS)...).CombinedOutput(); err != nil {
		return fmt.Errorf("unshare: %w: %s", err, out)
	}
	host, peer := vethNames(in.id)
	if err := ipBatch(nil, hostBatch(host, peer, p.netNS)); err != nil {
		return err
	}
	return ipBatch([]string{"--net=" + p.netNS}, instanceBatch(peer, in.addr, a.bits, a.gateway))
}

// ipBatch runs `ip -batch -`, under nsenter with these arguments if any.
func ipBatch(nsenter []string, lines []string) error {
	name, args := "ip", []string{"-batch", "-"}
	if nsenter != nil {
		name, args = "nsenter", append(append(append([]string{}, nsenter...), "--", "ip"), args...)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ip -batch: %w: %s", err, out)
	}
	return nil
}

// awaitReady waits for the healthcheck to pass, within its timeout, while the
// instance still has processes.
func (a *Agent) awaitReady(in *instance, spec *andenv1.ServiceSpec) error {
	deadline := time.Now().Add(time.Duration(spec.GetHealthcheckTimeoutSeconds()) * time.Second)
	target := net.JoinHostPort(in.addr, strconv.FormatUint(uint64(spec.GetPort()), 10))
	for {
		if err := in.ctx.Err(); err != nil {
			return err
		}
		populated, err := cgroupPopulated(in.paths.cgroup)
		if err != nil {
			return err
		}
		if !populated {
			return errors.New("the process exited before it was ready")
		}
		err = a.probe(in.ctx, target, spec.GetHealthcheck())
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("healthcheck did not pass in %d s: %w", spec.GetHealthcheckTimeoutSeconds(), err)
		}
		time.Sleep(readyPoll)
	}
}

// probe makes one healthcheck: an HTTP GET of its path or a TCP connect.
func (a *Agent) probe(ctx context.Context, target string, hc *andenv1.Healthcheck) error {
	ctx, cancel := context.WithTimeout(ctx, probeLimit)
	defer cancel()
	path, isHTTP := hc.GetCheck().(*andenv1.Healthcheck_Path)
	if !isHTTP {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+path.Path, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("healthcheck: %s", resp.Status)
	}
	return nil
}

// stop ends an instance with SIGTERM, then cgroup.kill, and removes everything
// it occupied. Until that is done it keeps being reported as it was: its address
// and processes are still in use.
func (a *Agent) stop(in *instance) {
	in.cancel()
	<-in.started
	p := in.paths
	if populated, _ := cgroupPopulated(p.cgroup); populated {
		// It fails when create never finished; cgroup.kill covers that.
		_ = exec.Command(a.cfg.Runtime, a.runtimeArgs("kill", in.id, "TERM")...).Run()
		if !waitEmpty(p.cgroup, stopGrace) {
			if err := killCgroup(p.cgroup); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Printf("instance %s: %v", in.id, err)
			}
			if !waitEmpty(p.cgroup, killTimeout) {
				log.Printf("instance %s: processes left in %s after cgroup.kill", in.id, p.cgroup)
			}
		}
	}
	_ = exec.Command(a.cfg.Runtime, a.runtimeArgs("delete", "--force", in.id)...).Run()
	a.teardown(in)
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.instances, in.id)
	a.changes++
}

// teardown removes what launch created. Each step may find nothing to do,
// because a start can fail or be cancelled at any point.
func (a *Agent) teardown(in *instance) {
	p := in.paths
	host, _ := vethNames(in.id)
	// Explicitly, and not through the namespace: the kernel destroys a netns
	// asynchronously, and the same instance id takes this name when it wakes.
	_ = exec.Command("ip", "link", "del", host).Run()
	for _, target := range []string{p.netNS, p.utsNS, p.ipcNS, p.merged} {
		err := syscall.Unmount(target, syscall.MNT_DETACH)
		if err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
			log.Printf("instance %s: unmount %s: %v", in.id, target, err)
		}
	}
	if err := os.RemoveAll(p.dir); err != nil {
		log.Printf("instance %s: %v", in.id, err)
	}
	if err := removeCgroup(p.cgroup); err != nil {
		log.Printf("instance %s: %v", in.id, err)
	}
}

func parsePopulated(events []byte) bool {
	return bytes.Contains(events, []byte("populated 1"))
}

// cgroupPopulated reads cgroup.events. Children of the cgroup do not exist, so
// this means the instance has processes.
func cgroupPopulated(dir string) (bool, error) {
	events, err := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	if err != nil {
		return false, err
	}
	return parsePopulated(events), nil
}

func killCgroup(dir string) error {
	return os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
}

func waitEmpty(dir string, timeout time.Duration) bool {
	for end := time.Now().Add(timeout); time.Now().Before(end); time.Sleep(readyPoll) {
		if populated, err := cgroupPopulated(dir); err != nil || !populated {
			return true
		}
	}
	return false
}

// removeCgroup retries while the kernel finishes with the last processes.
func removeCgroup(dir string) error {
	end := time.Now().Add(killTimeout)
	for {
		err := os.Remove(dir)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(end) {
			return err
		}
		time.Sleep(readyPoll)
	}
}
