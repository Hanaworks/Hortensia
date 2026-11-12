// Package compose adapts fixed package input to native Compose and Docker CLI.
package compose

import (
	"context"
	"docker-manager/internal/agent/executor"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Profile struct {
	DockerCLI, DockerSocket string
	Runtime                 ledger.RuntimeProfile
	ExecutionTimeout        time.Duration
	AllowLabelFile          bool
}
type Engine struct{ profile Profile }
type Prepared struct {
	Package       workspace.Package
	Evidence      ledger.PreparationEvidence
	Model         Model
	absoluteBinds map[string]bool
}
type Model struct {
	Services map[string]Service       `json:"services"`
	Networks map[string]NamedResource `json:"networks"`
	Volumes  map[string]NamedResource `json:"volumes"`
}
type Service struct {
	Image         string  `json:"image"`
	PullPolicy    string  `json:"pull_policy"`
	ContainerName string  `json:"container_name"`
	Volumes       []Mount `json:"volumes"`
}
type Mount struct {
	Type, Source, Target string
	ReadOnly             bool `json:"read_only"`
}
type NamedResource struct {
	Name     string `json:"name"`
	External bool   `json:"external"`
}

func New(p Profile) (*Engine, error) {
	if !filepath.IsAbs(p.DockerCLI) || !strings.HasPrefix(p.DockerSocket, "unix:///") || strings.ContainsAny(p.DockerSocket, "\x00\r\n") || p.ExecutionTimeout < 0 {
		return nil, errors.New("execution_profile_invalid")
	}
	if p.ExecutionTimeout == 0 {
		p.ExecutionTimeout = 1800 * time.Second
	}
	if p.ExecutionTimeout < time.Second || p.ExecutionTimeout > 24*time.Hour {
		return nil, errors.New("execution_profile_invalid")
	}
	return &Engine{p}, nil
}
func (e *Engine) environment(p workspace.Package) ([]string, error) {
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + filepath.Join(p.OperationDir, "home"), "DOCKER_CONFIG=" + filepath.Join(p.OperationDir, "docker-config"), "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	keys := make([]string, 0, len(p.Payload.Config.Env))
	for k := range p.Payload.Config.Env {
		if controlKey(k) {
			return nil, errors.New("execution_environment_key_forbidden")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+p.Payload.Config.Env[k])
	}
	return env, nil
}
func (e *Engine) command(p workspace.Package, args []string, timeout time.Duration) (executor.Command, error) {
	env, err := e.environment(p)
	if err != nil {
		return executor.Command{}, err
	}
	return executor.Command{Program: e.profile.DockerCLI, Args: append([]string{"--host", e.profile.DockerSocket}, args...), Dir: p.Dir, Env: env, Timeout: timeout}, nil
}
func (e *Engine) prefix(p workspace.Package) []string {
	args := []string{"compose", "--ansi", "never", "--project-name", p.Payload.ProjectName, "--project-directory", p.Dir}
	files := p.Payload.Config.EnvFiles
	if len(files) == 0 {
		for _, n := range workspace.Names(p) {
			if n == ".env" {
				files = []string{n}
			}
		}
	}
	if len(files) == 0 {
		args = append(args, "--env-file", filepath.Join(p.OperationDir, "empty.env"))
	} else {
		for _, f := range files {
			args = append(args, "--env-file", filepath.Join(p.Dir, filepath.FromSlash(f)))
		}
	}
	for _, f := range p.Payload.Config.ComposeFiles {
		args = append(args, "-f", filepath.Join(p.Dir, filepath.FromSlash(f)))
	}
	for _, v := range p.Payload.Config.Profiles {
		args = append(args, "--profile", v)
	}
	return args
}
func (e *Engine) Invocation(p workspace.Package) (executor.Command, error) {
	if err := workspace.Verify(context.Background(), p); err != nil {
		return executor.Command{}, err
	}
	if _, err := staticCheck(context.Background(), p, e.profile.AllowLabelFile); err != nil {
		return executor.Command{}, err
	}
	return e.command(p, append(e.prefix(p), "up", "--detach", "--no-build"), e.profile.ExecutionTimeout)
}
func (e *Engine) read(ctx context.Context, p workspace.Package, args ...string) ([]byte, error) {
	cmd, err := e.command(p, args, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return executor.RunReadOnly(ctx, cmd, 8<<20)
}
func (e *Engine) Probe(ctx context.Context) (ledger.RuntimeProfile, error) {
	p := e.profile.Runtime
	privateDir, err := os.MkdirTemp("", "docker-manager-probe-")
	if err != nil {
		return p, errors.New("docker_probe_workspace_failed")
	}
	home, config := filepath.Join(privateDir, "home"), filepath.Join(privateDir, "docker-config")
	defer func() { _ = os.Remove(home); _ = os.Remove(config); _ = os.Remove(privateDir) }()
	if os.Mkdir(home, 0700) != nil || os.Mkdir(config, 0700) != nil {
		return p, errors.New("docker_probe_workspace_failed")
	}
	cmd := executor.Command{Program: e.profile.DockerCLI, Args: []string{"--host", e.profile.DockerSocket, "info", "--format", "{{json .}}"}, Dir: privateDir, Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "DOCKER_CONFIG=" + config, "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}, Timeout: 30 * time.Second}
	return e.probeCommand(ctx, cmd)
}

// probePackage uses the same verified private HOME/config and explicit
// environment as the operation's read-only commands. It never runs config/up.
func (e *Engine) probePackage(ctx context.Context, p workspace.Package) (ledger.RuntimeProfile, error) {
	cmd, err := e.command(p, []string{"info", "--format", "{{json .}}"}, 30*time.Second)
	if err != nil {
		return e.profile.Runtime, err
	}
	return e.probeCommand(ctx, cmd)
}

func (e *Engine) probeCommand(ctx context.Context, cmd executor.Command) (ledger.RuntimeProfile, error) {
	p := e.profile.Runtime
	raw, err := executor.RunReadOnly(ctx, cmd, 8<<20)
	if err != nil {
		return p, errors.New("docker_probe_failed")
	}
	var info struct{ ID, ServerVersion string }
	if json.Unmarshal(raw, &info) != nil || info.ID == "" || info.ServerVersion == "" {
		return p, errors.New("docker_probe_invalid")
	}
	cmd.Args = []string{"--host", e.profile.DockerSocket, "compose", "version", "--short"}
	raw, err = executor.RunReadOnly(ctx, cmd, 1<<20)
	if err != nil {
		return p, errors.New("compose_probe_failed")
	}
	p.EngineID = info.ID
	p.DockerVersion = info.ServerVersion
	p.ComposeVersion = strings.TrimSpace(string(raw))
	if p.ComposeVersion == "" {
		return p, errors.New("compose_probe_invalid")
	}
	return p, nil
}
