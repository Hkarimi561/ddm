package docker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Info is the part of `docker info` ddm reports on.
type Info struct {
	ServerVersion   string
	OSType          string
	Architecture    string
	OperatingSystem string
	Driver          string
	DriverStatus    [][]string
	DockerRootDir   string
	MemTotal        int64
	NCPU            int
	ServerErrors    []string
	ClientInfo      struct {
		Version string
		Context string
		Plugins []Plugin
	}
}

// Plugin is a docker CLI plugin such as compose or model.
type Plugin struct {
	Name    string
	Version string
	Path    string
}

// ContainerdStore reports whether the engine uses the containerd image store.
func (i *Info) ContainerdStore() bool {
	for _, kv := range i.DriverStatus {
		if len(kv) == 2 && kv[0] == "driver-type" && strings.Contains(kv[1], "containerd") {
			return true
		}
	}
	return false
}

// Plugin returns the named CLI plugin, if installed.
func (i *Info) Plugin(name string) (Plugin, bool) {
	for _, p := range i.ClientInfo.Plugins {
		if p.Name == name {
			return p, true
		}
	}
	return Plugin{}, false
}

// Info runs `docker info`. When the engine is unreachable it still returns
// the client side (version, context, plugins) together with an error.
func (c *CLI) Info(ctx context.Context) (*Info, error) {
	out, msg, runErr := c.raw(ctx, "info", "--format", "{{json .}}")
	var info Info
	if out != "" {
		if err := json.Unmarshal([]byte(out), &info); err != nil && runErr == nil {
			return nil, err
		}
	}
	if len(info.ServerErrors) > 0 {
		return &info, errors.New(strings.Join(info.ServerErrors, "; "))
	}
	if runErr != nil {
		if msg == "" {
			msg = runErr.Error()
		}
		return &info, errors.New(msg)
	}
	return &info, nil
}
