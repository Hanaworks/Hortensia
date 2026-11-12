package compose

import (
	"bytes"
	"context"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func (e *Engine) Prepare(ctx context.Context, p workspace.Package, binding ledger.ProjectBinding) (Prepared, error) {
	result := Prepared{Package: p}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	absolute, err := staticCheck(ctx, p, e.profile.AllowLabelFile)
	if err != nil {
		return result, err
	}
	result.absoluteBinds = absolute
	if p.Payload.ExecutionProfile.EndpointID != e.profile.Runtime.EndpointID || p.Payload.ExecutionProfile.PolicyRevision != e.profile.Runtime.PolicyRevision || e.profile.Runtime.EngineID == "" {
		return result, errors.New("execution_profile_mismatch")
	}
	raw, err := e.read(ctx, p, append(e.prefix(p), "config", "--format", "json")...)
	if err != nil {
		return result, errors.New("compose_config_failed")
	}
	defer clear(raw)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&result.Model) != nil {
		return result, errors.New("compose_model_invalid")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return result, errors.New("compose_model_invalid")
	}
	if err = validateModel(ctx, result); err != nil {
		return result, err
	}
	pre, _, err := e.collect(ctx, p)
	if err != nil {
		return result, err
	}
	if binding.StackID != "" && (binding.StackID != p.Payload.StackID || binding.EndpointID != e.profile.Runtime.EndpointID || binding.ProjectName != p.Payload.ProjectName || binding.Status == "drifted") {
		return result, errors.New("resource_drift")
	}
	if !sameResources(pre.Resources, binding.Baseline.Resources) {
		return result, errors.New("resource_drift")
	}
	if err = e.dependencies(ctx, result, pre); err != nil {
		return result, err
	}
	expected := []ledger.ExpectedResource{}
	for service := range result.Model.Services {
		expected = append(expected, ledger.ExpectedResource{Kind: "container", LogicalKey: service})
	}
	for k, v := range result.Model.Networks {
		expected = append(expected, ledger.ExpectedResource{Kind: "network", LogicalKey: k, External: v.External})
	}
	for k, v := range result.Model.Volumes {
		expected = append(expected, ledger.ExpectedResource{Kind: "volume", LogicalKey: k, External: v.External})
	}
	sort.Slice(expected, func(i, j int) bool {
		return expected[i].Kind+expected[i].LogicalKey < expected[j].Kind+expected[j].LogicalKey
	})
	profile := e.profile.Runtime
	result.Evidence = ledger.PreparationEvidence{OperationID: p.OperationID, InputFingerprint: p.InputFingerprint, WorkspaceID: p.WorkspaceID, ManifestFingerprint: p.ManifestFingerprint, ProfileFingerprint: profile.Fingerprint, EngineID: profile.EngineID, DockerVersion: profile.DockerVersion, ComposeVersion: profile.ComposeVersion, OwnershipRevision: binding.Revision, Pre: pre, Expected: expected}
	return result, nil
}
func validateModel(ctx context.Context, p Prepared) error {
	if len(p.Model.Services) == 0 {
		return errors.New("compose_model_invalid")
	}
	for _, s := range p.Model.Services {
		if s.Image == "" || s.PullPolicy == "build" {
			return errors.New("compose_image_required")
		}
		for _, v := range s.Volumes {
			switch v.Type {
			case "bind":
				source := filepath.Clean(v.Source)
				rel, err := filepath.Rel(p.Package.Dir, source)
				inside := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
				if inside {
					if !v.ReadOnly {
						return errors.New("unsupported_bind_mount")
					}
					if _, err = workspace.ReadFile(ctx, p.Package, filepath.ToSlash(rel)); err != nil {
						return errors.New("unsupported_bind_mount")
					}
				} else {
					explicit := p.absoluteBinds[v.Source]
					for original := range p.absoluteBinds {
						if strings.HasPrefix(original, v.Source+":") {
							explicit = true
						}
					}
					if !filepath.IsAbs(v.Source) || !explicit {
						return errors.New("unsupported_bind_mount")
					}
				}
			case "volume", "tmpfs":
			default:
				return errors.New("unsupported_mount_type")
			}
		}
	}
	return nil
}
func (e *Engine) VerifyBeforeStart(ctx context.Context, p Prepared) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := workspace.Verify(ctx, p.Package); err != nil {
		return err
	}
	if _, err := staticCheck(ctx, p.Package, e.profile.AllowLabelFile); err != nil {
		return err
	}
	profile, err := e.probePackage(ctx, p.Package)
	if err != nil {
		return err
	}
	if profile.EngineID != p.Evidence.EngineID || profile.DockerVersion != p.Evidence.DockerVersion || profile.ComposeVersion != p.Evidence.ComposeVersion || e.profile.Runtime.Fingerprint != p.Evidence.ProfileFingerprint {
		return errors.New("execution_profile_mismatch")
	}
	pre, _, err := e.collect(ctx, p.Package)
	if err != nil {
		return err
	}
	if !sameResources(pre.Resources, p.Evidence.Pre.Resources) {
		return errors.New("resource_drift")
	}
	return e.dependencies(ctx, p, pre)
}
func sameResources(a, b []ledger.ResourceRecord) bool {
	normalize := func(in []ledger.ResourceRecord) []ledger.ResourceRecord {
		out := append([]ledger.ResourceRecord{}, in...)
		sort.Slice(out, func(i, j int) bool { return out[i].Kind+out[i].LogicalKey < out[j].Kind+out[j].LogicalKey })
		return out
	}
	x, _ := json.Marshal(normalize(a))
	y, _ := json.Marshal(normalize(b))
	return bytes.Equal(x, y)
}

func (e *Engine) named(ctx context.Context, p workspace.Package, kind, name string) ([]string, error) {
	var args []string
	name = regexp.QuoteMeta(name)
	switch kind {
	case "container":
		args = []string{"container", "ls", "--all", "--no-trunc", "--filter", "name=^/" + name + "$", "--format", "{{.ID}}"}
	case "network":
		args = []string{"network", "ls", "--no-trunc", "--filter", "name=^" + name + "$", "--format", "{{.ID}}"}
	case "volume":
		args = []string{"volume", "ls", "--filter", "name=^" + name + "$", "--format", "{{.Name}}"}
	}
	raw, err := e.read(ctx, p, args...)
	if err != nil {
		return nil, errors.New("resource_query_failed")
	}
	return parseLines(raw)
}
func (e *Engine) dependencies(ctx context.Context, p Prepared, pre ledger.Baseline) error {
	for kind, values := range map[string]map[string]NamedResource{"network": p.Model.Networks, "volume": p.Model.Volumes} {
		for _, v := range values {
			if v.Name == "" {
				return errors.New("compose_model_invalid")
			}
			ids, err := e.named(ctx, p.Package, kind, v.Name)
			if err != nil {
				return err
			}
			if v.External {
				if len(ids) != 1 {
					return errors.New("external_dependency_missing")
				}
				continue
			}
			for _, id := range ids {
				owned := false
				for _, r := range pre.Resources {
					if r.Kind == kind && (r.ResourceRef == id || (kind == "volume" && strings.HasPrefix(r.ResourceRef, id+"|"))) {
						owned = true
					}
				}
				if !owned {
					return errors.New("resource_name_conflict")
				}
			}
		}
	}
	for _, s := range p.Model.Services {
		if s.ContainerName == "" {
			continue
		}
		ids, err := e.named(ctx, p.Package, "container", s.ContainerName)
		if err != nil {
			return err
		}
		for _, id := range ids {
			owned := false
			for _, r := range pre.Resources {
				if r.Kind == "container" && r.ResourceRef == id {
					owned = true
				}
			}
			if !owned {
				return errors.New("container_name_conflict")
			}
		}
	}
	return nil
}
