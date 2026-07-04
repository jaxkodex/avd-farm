package farm

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// LabelSession and LabelDeadline mark emulator containers so avdd can find
// and reap them across restarts.
const (
	LabelSession  = "avd.session"
	LabelDeadline = "avd.deadline"
	Network       = "avd-net"
)

// ContainerInfo is what reconciliation needs to rebuild in-memory state from
// a live emulator container.
type ContainerInfo struct {
	SessionID string
	Deadline  int64
	ADBPort   int
	VNCPort   int
	API       int
}

// Docker is the subset of docker operations avdd performs. The real
// implementation shells out to the docker CLI; tests substitute a fake.
type Docker interface {
	// Run starts a detached container with the given `docker run` arguments.
	Run(ctx context.Context, args []string) error
	// Exec runs a shell command inside a container and returns its stdout.
	Exec(ctx context.Context, container, script string) (string, error)
	// Remove force-removes a container.
	Remove(ctx context.Context, container string) error
	// ListSessions returns every live container carrying the avd.session label.
	ListSessions(ctx context.Context) ([]ContainerInfo, error)
	// EnsureNetwork creates the avd-net network if it does not exist.
	EnsureNetwork(ctx context.Context) error
}

// CLIDocker implements Docker by invoking the `docker` binary via os/exec.
type CLIDocker struct{}

func (CLIDocker) run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (d CLIDocker) Run(ctx context.Context, args []string) error {
	_, err := d.run(ctx, append([]string{"run"}, args...)...)
	return err
}

func (d CLIDocker) Exec(ctx context.Context, container, script string) (string, error) {
	return d.run(ctx, "exec", container, "sh", "-c", script)
}

func (d CLIDocker) Remove(ctx context.Context, container string) error {
	_, err := d.run(ctx, "rm", "-f", container)
	return err
}

func (d CLIDocker) EnsureNetwork(ctx context.Context) error {
	if _, err := d.run(ctx, "network", "inspect", Network); err == nil {
		return nil
	}
	_, err := d.run(ctx, "network", "create", Network)
	return err
}

// inspectEntry mirrors the fields of `docker inspect` output that
// reconciliation reads.
type inspectEntry struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
		Env    []string          `json:"Env"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func (d CLIDocker) ListSessions(ctx context.Context) ([]ContainerInfo, error) {
	out, err := d.run(ctx, "ps", "--filter", "label="+LabelSession, "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = d.run(ctx, append([]string{"inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var entries []inspectEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return nil, fmt.Errorf("parsing docker inspect output: %w", err)
	}
	infos := make([]ContainerInfo, 0, len(entries))
	for _, e := range entries {
		info := ContainerInfo{SessionID: e.Config.Labels[LabelSession]}
		info.Deadline, _ = strconv.ParseInt(e.Config.Labels[LabelDeadline], 10, 64)
		for portProto, bindings := range e.NetworkSettings.Ports {
			if len(bindings) == 0 {
				continue
			}
			host, _ := strconv.Atoi(bindings[0].HostPort)
			switch {
			case strings.HasPrefix(portProto, "5555/"):
				info.ADBPort = host
			case strings.HasPrefix(portProto, "6080/"):
				info.VNCPort = host
			}
		}
		for _, env := range e.Config.Env {
			if v, ok := strings.CutPrefix(env, "API_LEVEL="); ok {
				info.API, _ = strconv.Atoi(v)
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}
