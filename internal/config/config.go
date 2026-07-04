// Package config loads and validates avdd's environment configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all avdd settings, populated from AVD_* environment variables.
type Config struct {
	Bind          string
	ADBPortLo     int
	ADBPortHi     int
	VNCPortLo     int
	VNCPortHi     int
	MaxDevices    int
	DefaultTTL    time.Duration
	EmulatorImage string
	Snapshot      string
	GPU           string
	PublishHost   string
	DeviceMemory  string
	BootTimeout   time.Duration
}

// FromEnv builds a Config from the environment, applying documented defaults
// and failing with a descriptive error when a required or malformed value is
// encountered.
func FromEnv() (*Config, error) {
	c := &Config{
		Bind:          envOr("AVD_BIND", "0.0.0.0:8700"),
		Snapshot:      envOr("AVD_SNAPSHOT", "golden"),
		GPU:           envOr("AVD_GPU", "swiftshader"),
		EmulatorImage: os.Getenv("AVD_EMULATOR_IMAGE"),
		PublishHost:   os.Getenv("AVD_PUBLISH_HOST"),
		DeviceMemory:  envOr("AVD_DEVICE_MEMORY", "4g"),
	}

	if c.EmulatorImage == "" {
		return nil, fmt.Errorf("AVD_EMULATOR_IMAGE is required: set it to the emulator container image to launch")
	}
	if c.PublishHost == "" {
		return nil, fmt.Errorf("AVD_PUBLISH_HOST is required: set it to the host/address clients will use to reach ADB and noVNC ports")
	}
	if c.GPU != "swiftshader" && c.GPU != "host" {
		return nil, fmt.Errorf("AVD_GPU must be \"swiftshader\" or \"host\", got %q", c.GPU)
	}

	var err error
	if c.ADBPortLo, c.ADBPortHi, err = parseRange(envOr("AVD_ADB_PORT_RANGE", "6000-6099")); err != nil {
		return nil, fmt.Errorf("AVD_ADB_PORT_RANGE: %w", err)
	}
	if c.VNCPortLo, c.VNCPortHi, err = parseRange(envOr("AVD_VNC_PORT_RANGE", "6100-6199")); err != nil {
		return nil, fmt.Errorf("AVD_VNC_PORT_RANGE: %w", err)
	}
	if c.MaxDevices, err = strconv.Atoi(envOr("AVD_MAX_DEVICES", "3")); err != nil || c.MaxDevices < 1 {
		return nil, fmt.Errorf("AVD_MAX_DEVICES must be a positive integer, got %q", envOr("AVD_MAX_DEVICES", "3"))
	}
	if c.DefaultTTL, err = time.ParseDuration(envOr("AVD_DEFAULT_TTL", "2h")); err != nil || c.DefaultTTL <= 0 {
		return nil, fmt.Errorf("AVD_DEFAULT_TTL must be a positive duration like \"2h\", got %q", envOr("AVD_DEFAULT_TTL", "2h"))
	}
	if c.BootTimeout, err = time.ParseDuration(envOr("AVD_BOOT_TIMEOUT", "3m")); err != nil || c.BootTimeout <= 0 {
		return nil, fmt.Errorf("AVD_BOOT_TIMEOUT must be a positive duration like \"3m\", got %q", envOr("AVD_BOOT_TIMEOUT", "3m"))
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseRange parses "LO-HI" into an inclusive port range.
func parseRange(s string) (lo, hi int, err error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected \"LO-HI\", got %q", s)
	}
	if lo, err = strconv.Atoi(strings.TrimSpace(parts[0])); err != nil {
		return 0, 0, fmt.Errorf("bad low port in %q", s)
	}
	if hi, err = strconv.Atoi(strings.TrimSpace(parts[1])); err != nil {
		return 0, 0, fmt.Errorf("bad high port in %q", s)
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, fmt.Errorf("invalid port range %q", s)
	}
	return lo, hi, nil
}
