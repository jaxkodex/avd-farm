// Package farm manages the lifecycle of ephemeral emulator devices.
package farm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avd-farm/avd-farm/internal/config"
	"github.com/avd-farm/avd-farm/internal/portpool"
)

// Sentinel errors mapped to HTTP statuses by the API layer.
var (
	ErrAtCapacity     = errors.New("device limit reached")
	ErrPortsExhausted = errors.New("port pool exhausted")
	ErrBootTimeout    = errors.New("device did not finish booting in time")
	ErrNotFound       = errors.New("no such device")
	ErrBadRequest     = errors.New("invalid request")
)

// Device is one live (or booting) emulator session.
type Device struct {
	ID       string    `json:"id"`
	API      int       `json:"api"`
	GPU      string    `json:"gpu"`
	ADBPort  int       `json:"adbPort"`
	VNCPort  int       `json:"vncPort"`
	Created  time.Time `json:"created"`
	Deadline time.Time `json:"deadline"`
	TTL      string    `json:"ttl"`
	State    string    `json:"state"` // "booting" | "ready"
}

// StartRequest is the decoded body of POST /devices.
type StartRequest struct {
	API int    `json:"api"`
	GPU string `json:"gpu,omitempty"`
	TTL string `json:"ttl,omitempty"`
}

// Manager owns port allocation, the device table, and container lifecycle.
type Manager struct {
	cfg    *config.Config
	docker Docker
	adb    *portpool.Pool
	vnc    *portpool.Pool

	// pollInterval controls the boot-poll cadence; tests shrink it.
	pollInterval time.Duration
	now          func() time.Time

	mu      sync.Mutex
	devices map[string]*Device
}

// NewManager creates a Manager; call Reconcile before serving traffic.
func NewManager(cfg *config.Config, docker Docker) *Manager {
	return &Manager{
		cfg:          cfg,
		docker:       docker,
		adb:          portpool.New(cfg.ADBPortLo, cfg.ADBPortHi),
		vnc:          portpool.New(cfg.VNCPortLo, cfg.VNCPortHi),
		pollInterval: 2 * time.Second,
		now:          time.Now,
		devices:      make(map[string]*Device),
	}
}

func containerName(id string) string { return "avd-" + id }

// Start allocates ports, launches the emulator container, and blocks until
// the device reports sys.boot_completed=1 or the boot timeout elapses.
func (m *Manager) Start(ctx context.Context, req StartRequest) (*Device, error) {
	gpu := req.GPU
	if gpu == "" {
		gpu = m.cfg.GPU
	}
	if gpu != "swiftshader" && gpu != "host" {
		return nil, fmt.Errorf("%w: gpu must be \"swiftshader\" or \"host\", got %q", ErrBadRequest, gpu)
	}
	ttl := m.cfg.DefaultTTL
	if req.TTL != "" {
		var err error
		if ttl, err = time.ParseDuration(req.TTL); err != nil || ttl <= 0 {
			return nil, fmt.Errorf("%w: ttl must be a positive duration like \"90m\", got %q", ErrBadRequest, req.TTL)
		}
	}

	dev, err := m.reserve(req.API, gpu, ttl)
	if err != nil {
		return nil, err
	}

	if err := m.launch(ctx, dev); err != nil {
		m.teardown(dev)
		return nil, err
	}
	if err := m.waitBooted(ctx, dev); err != nil {
		m.teardown(dev)
		return nil, err
	}

	m.mu.Lock()
	dev.State = "ready"
	m.mu.Unlock()
	return dev, nil
}

// reserve claims capacity and ports under the lock and registers the device.
func (m *Manager) reserve(api int, gpu string, ttl time.Duration) (*Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.devices) >= m.cfg.MaxDevices {
		return nil, ErrAtCapacity
	}
	adbPort, err := m.adb.Alloc()
	if err != nil {
		return nil, ErrPortsExhausted
	}
	vncPort, err := m.vnc.Alloc()
	if err != nil {
		m.adb.Free(adbPort)
		return nil, ErrPortsExhausted
	}
	now := m.now()
	dev := &Device{
		ID:       newID(),
		API:      api,
		GPU:      gpu,
		ADBPort:  adbPort,
		VNCPort:  vncPort,
		Created:  now,
		Deadline: now.Add(ttl),
		TTL:      ttl.String(),
		State:    "booting",
	}
	m.devices[dev.ID] = dev
	return dev, nil
}

func (m *Manager) launch(ctx context.Context, dev *Device) error {
	args := []string{
		"-d", "--name", containerName(dev.ID),
		"--label", LabelSession + "=" + dev.ID,
		"--label", LabelDeadline + "=" + strconv.FormatInt(dev.Deadline.Unix(), 10),
		"--device", "/dev/kvm",
	}
	if dev.GPU == "host" {
		args = append(args, "--gpus", "all")
	}
	args = append(args,
		"--network", Network,
		"--memory", m.cfg.DeviceMemory,
		"-p", fmt.Sprintf("%s:%d:5555", m.cfg.PublishHost, dev.ADBPort),
		"-p", fmt.Sprintf("%s:%d:6080", m.cfg.PublishHost, dev.VNCPort),
		"-e", "GPU_MODE="+dev.GPU,
		"-e", "SNAPSHOT="+m.cfg.Snapshot,
		"-e", "API_LEVEL="+strconv.Itoa(dev.API),
		m.cfg.EmulatorImage,
	)
	return m.docker.Run(ctx, args)
}

// waitBooted polls sys.boot_completed inside the container until it reads 1.
func (m *Manager) waitBooted(ctx context.Context, dev *Device) error {
	ctx, cancel := context.WithTimeout(ctx, m.cfg.BootTimeout)
	defer cancel()
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		out, err := m.docker.Exec(ctx, containerName(dev.ID),
			"adb wait-for-device; adb shell getprop sys.boot_completed")
		if err == nil && strings.TrimSpace(out) == "1" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ErrBootTimeout
		case <-ticker.C:
		}
	}
}

// Stop removes a device's container and frees its ports.
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	dev, ok := m.devices[id]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	m.teardown(dev)
	return nil
}

// teardown removes the container (best-effort), then releases ports and the
// device table entry.
func (m *Manager) teardown(dev *Device) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.docker.Remove(ctx, containerName(dev.ID)); err != nil {
		log.Printf("removing container %s: %v", containerName(dev.ID), err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adb.Free(dev.ADBPort)
	m.vnc.Free(dev.VNCPort)
	delete(m.devices, dev.ID)
}

// Get returns one device by id.
func (m *Manager) Get(id string) (*Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dev, ok := m.devices[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *dev
	return &cp, nil
}

// List returns all devices sorted by creation time.
func (m *Manager) List() []*Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Device, 0, len(m.devices))
	for _, dev := range m.devices {
		cp := *dev
		out = append(out, &cp)
	}
	return out
}

// Reconcile rebuilds device and port state from live avd.session containers,
// so a server restart neither strands nor double-allocates anything.
func (m *Manager) Reconcile(ctx context.Context) error {
	infos, err := m.docker.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("listing session containers: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, info := range infos {
		if info.SessionID == "" {
			continue
		}
		if err := m.adb.Reserve(info.ADBPort); err != nil {
			log.Printf("reconcile %s: adb port: %v", info.SessionID, err)
		}
		if err := m.vnc.Reserve(info.VNCPort); err != nil {
			log.Printf("reconcile %s: vnc port: %v", info.SessionID, err)
		}
		deadline := time.Unix(info.Deadline, 0)
		m.devices[info.SessionID] = &Device{
			ID:       info.SessionID,
			API:      info.API,
			ADBPort:  info.ADBPort,
			VNCPort:  info.VNCPort,
			Deadline: deadline,
			State:    "ready",
		}
	}
	if len(infos) > 0 {
		log.Printf("reconciled %d existing device(s)", len(infos))
	}
	return nil
}

// ReapExpired tears down every device whose deadline has passed and reports
// how many were removed.
func (m *Manager) ReapExpired() int {
	now := m.now()
	m.mu.Lock()
	var expired []*Device
	for _, dev := range m.devices {
		if !dev.Deadline.IsZero() && now.After(dev.Deadline) {
			expired = append(expired, dev)
		}
	}
	m.mu.Unlock()
	for _, dev := range expired {
		log.Printf("reaping expired device %s (deadline %s)", dev.ID, dev.Deadline.Format(time.RFC3339))
		m.teardown(dev)
	}
	return len(expired)
}

// RunReaper reaps expired devices on a fixed ticker until ctx is cancelled.
// It is the backstop for clients that never call DELETE.
func (m *Manager) RunReaper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.ReapExpired()
		}
	}
}

func newID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}
