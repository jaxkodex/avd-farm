package farm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avd-farm/avd-farm/internal/config"
)

// fakeDocker records lifecycle calls and simulates boot polling.
type fakeDocker struct {
	mu       sync.Mutex
	runs     [][]string
	removed  []string
	sessions []ContainerInfo
	// bootAfter is how many Exec polls return "0" before "1"; -1 never boots.
	bootAfter int
	execCount int
}

func (f *fakeDocker) Run(ctx context.Context, args []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, args)
	return nil
}

func (f *fakeDocker) Exec(ctx context.Context, container, script string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCount++
	if f.bootAfter >= 0 && f.execCount > f.bootAfter {
		return "1\n", nil
	}
	return "0\n", nil
}

func (f *fakeDocker) Remove(ctx context.Context, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, container)
	return nil
}

func (f *fakeDocker) ListSessions(ctx context.Context) ([]ContainerInfo, error) {
	return f.sessions, nil
}

func (f *fakeDocker) EnsureNetwork(ctx context.Context) error { return nil }

func testConfig() *config.Config {
	return &config.Config{
		ADBPortLo:     6000,
		ADBPortHi:     6001,
		VNCPortLo:     6100,
		VNCPortHi:     6101,
		MaxDevices:    2,
		DefaultTTL:    time.Hour,
		EmulatorImage: "example/emulator:test",
		Snapshot:      "golden",
		GPU:           "swiftshader",
		PublishHost:   "203.0.113.1",
		DeviceMemory:  "4g",
		BootTimeout:   200 * time.Millisecond,
	}
}

func newTestManager(cfg *config.Config, d Docker) *Manager {
	m := NewManager(cfg, d)
	m.pollInterval = time.Millisecond
	return m
}

func TestStartBuildsRunArgs(t *testing.T) {
	fd := &fakeDocker{}
	m := newTestManager(testConfig(), fd)

	dev, err := m.Start(context.Background(), StartRequest{API: 34, GPU: "host", TTL: "90m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if dev.State != "ready" {
		t.Errorf("State = %q, want ready", dev.State)
	}
	if len(fd.runs) != 1 {
		t.Fatalf("expected 1 docker run, got %d", len(fd.runs))
	}
	args := strings.Join(fd.runs[0], " ")
	for _, want := range []string{
		"--name avd-" + dev.ID,
		"--label avd.session=" + dev.ID,
		"--device /dev/kvm",
		"--gpus all",
		"--network avd-net",
		"--memory 4g",
		"-p 203.0.113.1:6000:5555",
		"-p 203.0.113.1:6100:6080",
		"-e GPU_MODE=host",
		"-e SNAPSHOT=golden",
		"-e API_LEVEL=34",
		"example/emulator:test",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("docker run args missing %q\nargs: %s", want, args)
		}
	}
	if wantTTL := 90 * time.Minute; dev.Deadline.Sub(dev.Created) != wantTTL {
		t.Errorf("deadline-created = %v, want %v", dev.Deadline.Sub(dev.Created), wantTTL)
	}
}

func TestStartSwiftshaderOmitsGPUs(t *testing.T) {
	fd := &fakeDocker{}
	m := newTestManager(testConfig(), fd)
	if _, err := m.Start(context.Background(), StartRequest{API: 34}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if args := strings.Join(fd.runs[0], " "); strings.Contains(args, "--gpus") {
		t.Errorf("swiftshader run should not include --gpus: %s", args)
	}
}

func TestStartCapacityAndExhaustion(t *testing.T) {
	cfg := testConfig()
	cfg.MaxDevices = 3 // higher than the 2-port pools, so exhaustion hits first
	m := newTestManager(cfg, &fakeDocker{})

	for range 2 {
		if _, err := m.Start(context.Background(), StartRequest{API: 34}); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	if _, err := m.Start(context.Background(), StartRequest{API: 34}); !errors.Is(err, ErrPortsExhausted) {
		t.Fatalf("expected ErrPortsExhausted, got %v", err)
	}

	cfg2 := testConfig()
	cfg2.MaxDevices = 1
	m2 := newTestManager(cfg2, &fakeDocker{})
	if _, err := m2.Start(context.Background(), StartRequest{API: 34}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m2.Start(context.Background(), StartRequest{API: 34}); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("expected ErrAtCapacity, got %v", err)
	}
}

func TestStartBootTimeoutTearsDown(t *testing.T) {
	fd := &fakeDocker{bootAfter: -1}
	m := newTestManager(testConfig(), fd)

	_, err := m.Start(context.Background(), StartRequest{API: 34})
	if !errors.Is(err, ErrBootTimeout) {
		t.Fatalf("expected ErrBootTimeout, got %v", err)
	}
	if len(fd.removed) != 1 {
		t.Fatalf("expected the failed container to be removed, removed=%v", fd.removed)
	}
	if len(m.List()) != 0 {
		t.Error("device table should be empty after boot timeout")
	}
	if m.adb.InUse() != 0 || m.vnc.InUse() != 0 {
		t.Error("ports should be freed after boot timeout")
	}
}

func TestStopFreesPortsAndRemoves(t *testing.T) {
	fd := &fakeDocker{}
	m := newTestManager(testConfig(), fd)
	dev, err := m.Start(context.Background(), StartRequest{API: 34})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(dev.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !slices.Contains(fd.removed, "avd-"+dev.ID) {
		t.Errorf("container avd-%s not removed: %v", dev.ID, fd.removed)
	}
	if _, err := m.Get(dev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Stop: want ErrNotFound, got %v", err)
	}
	if m.adb.InUse() != 0 || m.vnc.InUse() != 0 {
		t.Error("ports not returned to pool after Stop")
	}
	if err := m.Stop(dev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Stop: want ErrNotFound, got %v", err)
	}
}

func TestConcurrentTeardownFreesPortsOnce(t *testing.T) {
	fd := &fakeDocker{}
	m := newTestManager(testConfig(), fd)
	dev, err := m.Start(context.Background(), StartRequest{API: 34})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Race two teardowns (e.g. DELETE vs reaper); only one may free ports.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); m.teardown(dev) }()
	}
	wg.Wait()

	// If the second teardown also freed, allocating both pools twice would
	// succeed on a 2-port pool that had one port claimed by a new device.
	next, err := m.Start(context.Background(), StartRequest{API: 34})
	if err != nil {
		t.Fatalf("Start after teardown: %v", err)
	}
	m.teardown(dev) // stale re-teardown must not free next's ports
	if _, err := m.Get(next.ID); err != nil {
		t.Fatalf("live device lost after stale teardown: %v", err)
	}
	if m.adb.InUse() != 1 || m.vnc.InUse() != 1 {
		t.Errorf("ports in use = %d/%d, want 1/1", m.adb.InUse(), m.vnc.InUse())
	}
}

func TestReapExpired(t *testing.T) {
	fd := &fakeDocker{}
	m := newTestManager(testConfig(), fd)

	clock := time.Now()
	m.now = func() time.Time { return clock }

	live, err := m.Start(context.Background(), StartRequest{API: 34, TTL: "2h"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	doomed, err := m.Start(context.Background(), StartRequest{API: 34, TTL: "10m"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if n := m.ReapExpired(); n != 0 {
		t.Fatalf("nothing should be expired yet, reaped %d", n)
	}

	clock = clock.Add(11 * time.Minute)
	if n := m.ReapExpired(); n != 1 {
		t.Fatalf("expected 1 reaped device, got %d", n)
	}
	if _, err := m.Get(doomed.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired device %s still present", doomed.ID)
	}
	if _, err := m.Get(live.ID); err != nil {
		t.Errorf("live device %s was reaped early: %v", live.ID, err)
	}
	if !slices.Contains(fd.removed, "avd-"+doomed.ID) {
		t.Errorf("expired container not removed: %v", fd.removed)
	}
}

func TestReconcileRebuildsState(t *testing.T) {
	fd := &fakeDocker{sessions: []ContainerInfo{
		{SessionID: "abc123", Deadline: time.Now().Add(time.Hour).Unix(), ADBPort: 6001, VNCPort: 6101, API: 34},
	}}
	m := newTestManager(testConfig(), fd)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	dev, err := m.Get("abc123")
	if err != nil {
		t.Fatalf("Get after Reconcile: %v", err)
	}
	if dev.ADBPort != 6001 || dev.VNCPort != 6101 {
		t.Errorf("ports = %d/%d, want 6001/6101", dev.ADBPort, dev.VNCPort)
	}

	// The reconciled ports must not be handed out again: with 2-port pools,
	// one Start succeeds on the remaining ports and the next exhausts.
	first, err := m.Start(context.Background(), StartRequest{API: 34})
	if err != nil {
		t.Fatalf("Start after Reconcile: %v", err)
	}
	if first.ADBPort == 6001 || first.VNCPort == 6101 {
		t.Errorf("Start reused reconciled ports: %d/%d", first.ADBPort, first.VNCPort)
	}
}
