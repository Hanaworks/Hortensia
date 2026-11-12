package compose

import (
	"context"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ResourceLimit = 200

type associationError struct{ Resource ledger.ResourceRecord }

func (e *associationError) Error() string { return "resource_association_invalid" }

type resourceItem struct {
	Kind    string  `json:"resource_kind"`
	ID      string  `json:"resource_id"`
	Name    string  `json:"name"`
	Project *string `json:"project_name"`
	Service *string `json:"service_name"`
	Stack   *string `json:"stack_id"`
	Image   *string `json:"image_id"`
	State   string  `json:"state"`
	Health  string  `json:"health"`
}
type observation struct {
	Status      string            `json:"status"`
	CollectedAt string            `json:"collected_at"`
	Items       []resourceItem    `json:"items"`
	Error       *observationError `json:"error"`
}
type observationError struct {
	Category string `json:"category"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Retry    string `json:"retry_action"`
}
type inspected struct {
	ID                                      string `json:"Id"`
	Name, Created, CreatedAt, Driver, Image string
	Labels                                  map[string]string
	Config                                  struct{ Labels map[string]string }
	State                                   struct {
		Status string
		Health *struct{ Status string }
	}
}

func nowTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
func parseLines(raw []byte) ([]string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return []string{}, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > ResourceLimit {
		return nil, errors.New("resource_budget_exceeded")
	}
	seen := map[string]bool{}
	for i, v := range lines {
		v = strings.TrimSpace(v)
		if v == "" || strings.ContainsAny(v, " \t\r\x00") || seen[v] {
			return nil, errors.New("resource_list_invalid")
		}
		seen[v] = true
		lines[i] = v
	}
	sort.Strings(lines)
	return lines, nil
}
func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (e *Engine) collect(ctx context.Context, p workspace.Package) (ledger.Baseline, []resourceItem, error) {
	base := ledger.Baseline{CollectedAt: nowTimestamp(), Resources: []ledger.ResourceRecord{}}
	items := []resourceItem{}
	keys := map[string]bool{}
	total := 0
	for _, kind := range []string{"container", "network", "volume"} {
		args := []string{kind, "ls"}
		if kind == "container" {
			args = append(args, "--all", "--no-trunc")
		} else if kind == "network" {
			args = append(args, "--no-trunc")
		}
		format := "{{.ID}}"
		if kind == "volume" {
			format = "{{.Name}}"
		}
		args = append(args, "--filter", "label=com.docker.compose.project="+p.Payload.ProjectName, "--format", format)
		raw, err := e.read(ctx, p, args...)
		if err != nil {
			return base, items, errors.New("resource_query_failed")
		}
		refs, err := parseLines(raw)
		if err != nil {
			return base, items, err
		}
		total += len(refs)
		if total > ResourceLimit {
			return base, items, errors.New("resource_budget_exceeded")
		}
		for _, ref := range refs {
			if kind != "volume" && !validID(ref) {
				return base, items, errors.New("resource_list_invalid")
			}
			raw, err = e.read(ctx, p, kind, "inspect", ref)
			if err != nil {
				return base, items, errors.New("resource_inspect_failed")
			}
			var values []inspected
			if json.Unmarshal(raw, &values) != nil || len(values) != 1 {
				return base, items, errors.New("resource_inspect_invalid")
			}
			v := values[0]
			labels := v.Labels
			if kind == "container" {
				labels = v.Config.Labels
			}
			if labels["com.docker.compose.project"] != p.Payload.ProjectName {
				return base, items, &associationError{ledger.ResourceRecord{Kind: kind, ResourceRef: ref, ProjectName: labels["com.docker.compose.project"]}}
			}
			r := ledger.ResourceRecord{Kind: kind, ResourceRef: ref, Classification: "managed", CreatedAt: v.Created, ProjectName: p.Payload.ProjectName, Driver: v.Driver}
			switch kind {
			case "container":
				if v.ID != ref || v.Name == "" {
					return base, items, errors.New("resource_inspect_invalid")
				}
				r.Service = labels["com.docker.compose.service"]
				r.ContainerNumber, err = strconv.Atoi(labels["com.docker.compose.container-number"])
				if err != nil || r.ContainerNumber < 1 || r.Service == "" {
					return base, items, &associationError{r}
				}
				r.LogicalKey = fmt.Sprintf("%s/%d", r.Service, r.ContainerNumber)
				state := v.State.Status
				switch state {
				case "created", "restarting", "running", "removing", "paused", "exited", "dead":
				default:
					state = "unknown"
				}
				health := "none"
				if v.State.Health != nil {
					health = v.State.Health.Status
					switch health {
					case "starting", "healthy", "unhealthy":
					default:
						health = "unknown"
					}
				}
				project, service := p.Payload.ProjectName, r.Service
				// Labels identify candidates, never a local Stack registration.
				item := resourceItem{Kind: "container", ID: v.ID, Name: strings.TrimPrefix(v.Name, "/"), Project: &project, Service: &service, State: state, Health: health}
				if strings.HasPrefix(v.Image, "sha256:") && validID(strings.TrimPrefix(v.Image, "sha256:")) {
					image := v.Image
					item.Image = &image
				}
				items = append(items, item)
			case "network":
				if v.ID != ref {
					return base, items, errors.New("resource_inspect_invalid")
				}
				r.LogicalKey = labels["com.docker.compose.network"]
			case "volume":
				if v.Name != ref || v.Driver == "" {
					return base, items, errors.New("resource_inspect_invalid")
				}
				r.CreatedAt = v.CreatedAt
				r.ResourceRef = v.Name + "|" + v.Driver + "|" + v.CreatedAt
				r.LogicalKey = labels["com.docker.compose.volume"]
			}
			key := kind + "\x00" + r.LogicalKey
			if r.LogicalKey == "" || keys[key] {
				return base, items, &associationError{r}
			}
			keys[key] = true
			base.Resources = append(base.Resources, r)
		}
	}
	sort.Slice(base.Resources, func(i, j int) bool {
		return base.Resources[i].Kind+base.Resources[i].LogicalKey < base.Resources[j].Kind+base.Resources[j].LogicalKey
	})
	return base, items, nil
}
func (e *Engine) Observe(ctx context.Context, p Prepared) (ledger.ObservationEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := workspace.Verify(ctx, p.Package); err != nil {
		return observationFailure("workspace_drift", err)
	}
	if !e.observationContextMatches(p) {
		return observationFailure("execution_profile_mismatch", errors.New("execution_profile_mismatch"))
	}
	actual, err := e.probePackage(ctx, p.Package)
	if err != nil {
		return observationFailure(err.Error(), err)
	}
	if actual.EngineID != p.Evidence.EngineID || actual.DockerVersion != p.Evidence.DockerVersion || actual.ComposeVersion != p.Evidence.ComposeVersion {
		return observationFailure("execution_profile_mismatch", errors.New("execution_profile_mismatch"))
	}
	post, items, err := e.collect(ctx, p.Package)
	result := ledger.ObservationEvidence{Post: post}
	o := observation{Status: "complete", CollectedAt: post.CollectedAt, Items: items}
	if err != nil {
		var conflict *associationError
		if errors.As(err, &conflict) {
			result.Conflicts = append(result.Conflicts, conflict.Resource)
		}
		code := "resource_query_failed"
		if err.Error() == "resource_budget_exceeded" {
			code = "resource_budget_exceeded"
			o.Status = "incomplete"
		} else {
			o.Status = "error"
		}
		o.Error = &observationError{"unavailable", code, "Resource observation could not be completed.", "query_operation"}
		result.Observation, _ = json.Marshal(o)
		return result, err
	}
	pre := map[string]ledger.ResourceRecord{}
	for _, r := range p.Evidence.Pre.Resources {
		pre[r.Kind+"\x00"+r.LogicalKey] = r
	}
	expected := map[string]ledger.ExpectedResource{}
	for _, r := range p.Evidence.Expected {
		expected[r.Kind+"\x00"+r.LogicalKey] = r
	}
	for _, r := range post.Resources {
		key := r.Kind + "\x00" + r.LogicalKey
		old, existed := pre[key]
		want, ok := expected[key]
		if r.Kind == "container" {
			want, ok = expected[r.Kind+"\x00"+r.Service]
		}
		if !existed && (!ok || want.External) {
			result.Conflicts = append(result.Conflicts, r)
			continue
		}
		if existed && old.ResourceRef == r.ResourceRef && old != r {
			result.Conflicts = append(result.Conflicts, r)
			continue
		}
		if existed && old.ResourceRef != r.ResourceRef {
			if !ok || want.External || old.Service != r.Service || old.ContainerNumber != r.ContainerNumber {
				result.Conflicts = append(result.Conflicts, r)
				continue
			}
			result.Replacements = append(result.Replacements, ledger.Replacement{Kind: r.Kind, LogicalKey: r.LogicalKey, OldRef: old.ResourceRef, NewRef: r.ResourceRef})
		}
	}
	// Evidence must be read from the authenticated ledger preparation, after a
	// successful CommitPrepared (also after reopening). Revision zero has never
	// published an active baseline. New/replaced candidates remain unassociated
	// publicly until publication; only unchanged exact Pre identities qualify.
	if p.Evidence.OwnershipRevision > 0 {
		conflicted := map[string]bool{}
		for _, r := range result.Conflicts {
			conflicted[r.Kind+"\x00"+r.ResourceRef] = true
		}
		known := map[string]bool{}
		for _, r := range post.Resources {
			old, ok := pre[r.Kind+"\x00"+r.LogicalKey]
			if ok && r.Kind == "container" && r.Classification == "managed" && r == old && !conflicted[r.Kind+"\x00"+r.ResourceRef] {
				known[r.ResourceRef] = true
			}
		}
		for i := range o.Items {
			if known[o.Items[i].ID] {
				stack := p.Package.Payload.StackID
				o.Items[i].Stack = &stack
			}
		}
	}
	result.Observation, _ = json.Marshal(o)
	return result, nil
}

func (e *Engine) observationContextMatches(p Prepared) bool {
	profile := e.profile.Runtime
	proof := p.Evidence
	pkg := p.Package
	return profile.EndpointID != "" && profile.Fingerprint != "" && profile.EngineID != "" && profile.DockerVersion != "" && profile.ComposeVersion != "" && pkg.InputFingerprint != "" && pkg.Payload.StackID != "" && pkg.Payload.ProjectName != "" &&
		pkg.Payload.ExecutionProfile.EndpointID == profile.EndpointID && pkg.Payload.ExecutionProfile.PolicyRevision == profile.PolicyRevision &&
		proof.OperationID == pkg.OperationID && proof.InputFingerprint == pkg.InputFingerprint && proof.WorkspaceID == pkg.WorkspaceID && proof.ManifestFingerprint == pkg.ManifestFingerprint &&
		proof.ProfileFingerprint == profile.Fingerprint && proof.EngineID == profile.EngineID && proof.DockerVersion == profile.DockerVersion && proof.ComposeVersion == profile.ComposeVersion
}

func observationFailure(code string, cause error) (ledger.ObservationEvidence, error) {
	category, retry := "unavailable", "query_operation"
	if code == "execution_profile_mismatch" || code == "workspace_drift" {
		category, retry = "reconciliation_required", "refresh_and_review"
	}
	post := ledger.Baseline{CollectedAt: nowTimestamp(), Resources: []ledger.ResourceRecord{}}
	view := observation{Status: "error", CollectedAt: post.CollectedAt, Items: []resourceItem{}, Error: &observationError{category, code, "Resource observation could not be completed.", retry}}
	raw, _ := json.Marshal(view)
	return ledger.ObservationEvidence{Observation: raw, Post: post}, cause
}
