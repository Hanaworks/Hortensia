package compose

import (
	"context"
	"docker-manager/internal/agent/ledger"
	"docker-manager/internal/agent/workspace"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const containerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const networkID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func init() {
	if os.Getenv("M1_CLI_FIXTURE") != "1" {
		return
	}
	args := os.Args[1:]
	record := os.Getenv("M1_FIXTURE_RECORD")
	f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(7)
	}
	raw, _ := json.Marshal(struct{ Args, Env []string }{args, os.Environ()})
	fmt.Fprintln(f, string(raw))
	f.Close()
	joined := strings.Join(args, " ")
	scenario := os.Getenv("M1_FIXTURE_SCENARIO")
	post := scenario == "post" || scenario == "disappear" || scenario == "association"
	switch {
	case strings.Contains(joined, "info --format"):
		if scenario == "probefailed" {
			os.Exit(4)
		}
		id, version := "engine-id", "27.0"
		if scenario == "enginechanged" {
			id = "other-engine"
		}
		if scenario == "dockerchanged" {
			version = "28.changed"
		}
		fmt.Printf(`{"ID":%q,"ServerVersion":%q}`, id, version)
	case strings.Contains(joined, "compose version --short"):
		if scenario == "versionchanged" {
			fmt.Print("v2.changed")
		} else {
			fmt.Print("v2.test")
		}
	case strings.Contains(joined, "config --format json"):
		if scenario == "externalmissing" || scenario == "externalsuccess" {
			fmt.Print(`{"services":{"app":{"image":"alpine"}},"networks":{"shared":{"name":"shared","external":true}}}`)
		} else if scenario == "volumecollision" {
			fmt.Print(`{"services":{"app":{"image":"alpine"}},"volumes":{"data":{"name":"demo_data"}}}`)
		} else if scenario == "missingimage" {
			fmt.Print(`{"services":{"app":{}}}`)
		} else {
			fmt.Print(`{"services":{"app":{"image":"alpine"}},"networks":{"default":{"name":"demo_default"}}}`)
		}
	case strings.Contains(joined, "container ls"):
		if scenario == "unreachable" {
			os.Exit(4)
		}
		if post {
			fmt.Println(containerID)
		}
	case strings.Contains(joined, "network ls"):
		if post || scenario == "externalsuccess" && strings.Contains(joined, "--filter name=") {
			fmt.Println(networkID)
		}
	case strings.Contains(joined, "volume ls"):
		if scenario == "volumecollision" && strings.Contains(joined, "--filter name=") {
			fmt.Println("demo_data")
		}
	case strings.Contains(joined, "container inspect"):
		if scenario == "disappear" {
			os.Exit(4)
		}
		service := "app"
		if scenario == "association" {
			service = ""
		}
		fmt.Printf(`[{"Id":%q,"Name":"/demo-app-1","Image":%q,"Created":"2026-11-12T00:00:00Z","Config":{"Labels":{"com.docker.compose.project":"demo","com.docker.compose.service":%q,"com.docker.compose.container-number":"1"}},"State":{"Status":"exited"}}]`, containerID, "sha256:"+networkID, service)
	case strings.Contains(joined, "network inspect"):
		fmt.Printf(`[{"Id":%q,"Name":"demo_default","Created":"2026-11-12T00:00:00Z","Driver":"bridge","Labels":{"com.docker.compose.project":"demo","com.docker.compose.network":"default"}}]`, networkID)
	default:
		os.Exit(8)
	}
	os.Exit(0)
}
func testPackage(t *testing.T, yamlText string, extra map[string]string, scenario string) (workspace.Package, *Engine, string) {
	t.Helper()
	state := t.TempDir()
	record := filepath.Join(t.TempDir(), "calls.jsonl")
	payload := workspace.Payload{OperationType: "stack.apply", StackID: "11111111-1111-4111-8111-111111111111", VersionID: "22222222-2222-4222-8222-222222222222", ProjectName: "demo"}
	payload.ExecutionProfile.EndpointID = "endpoint"
	payload.ExecutionProfile.PolicyRevision = 1
	payload.Config.Files = []workspace.File{{Path: "compose.yaml", ContentB64: base64.StdEncoding.EncodeToString([]byte(yamlText))}}
	payload.Config.ComposeFiles = []string{"compose.yaml"}
	payload.Config.Env = map[string]string{"M1_CLI_FIXTURE": "1", "M1_FIXTURE_RECORD": record, "M1_FIXTURE_SCENARIO": scenario}
	for n, s := range extra {
		payload.Config.Files = append(payload.Config.Files, workspace.File{Path: n, ContentB64: base64.StdEncoding.EncodeToString([]byte(s))})
		if strings.HasSuffix(n, ".yaml") {
			payload.Config.ComposeFiles = append(payload.Config.ComposeFiles, n)
		}
	}
	raw, _ := json.Marshal(payload)
	p, err := workspace.Materialize(context.Background(), state, workspace.Input{OperationID: "33333333-3333-4333-8333-333333333333", InputFingerprint: "fingerprint", Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	e, err := New(Profile{DockerCLI: exe, DockerSocket: "unix:///var/run/docker.sock", Runtime: ledger.RuntimeProfile{EndpointID: "endpoint", PolicyRevision: 1, Fingerprint: "profile", EngineID: "engine-id", DockerVersion: "27.0", ComposeVersion: "v2.test"}, ExecutionTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return p, e, record
}
func calls(t *testing.T, file string) []struct{ Args, Env []string } {
	t.Helper()
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []struct{ Args, Env []string }
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var v struct{ Args, Env []string }
		if json.Unmarshal([]byte(line), &v) != nil {
			t.Fatal("bad call record")
		}
		out = append(out, v)
	}
	return out
}

// This fixture fills the identity that production reads back from the ledger
// after CommitPrepared. Tests cannot substitute a runtime result provider.
func observationInput(p workspace.Package, e *Engine, partial ledger.PreparationEvidence) Prepared {
	profile := e.profile.Runtime
	partial.OperationID = p.OperationID
	partial.InputFingerprint = p.InputFingerprint
	partial.WorkspaceID = p.WorkspaceID
	partial.ManifestFingerprint = p.ManifestFingerprint
	partial.ProfileFingerprint = profile.Fingerprint
	partial.EngineID = profile.EngineID
	partial.DockerVersion = profile.DockerVersion
	partial.ComposeVersion = profile.ComposeVersion
	if partial.Pre.CollectedAt == "" {
		partial.Pre.CollectedAt = nowTimestamp()
	}
	return Prepared{Package: p, Evidence: partial}
}
func TestUnsafeAllReferencesRejectBeforeFirstCLI(t *testing.T) {
	for name, input := range map[string]string{
		"alias":     "x-env: &env /outside\nservices:\n  app: {image: alpine, env_file: *env}",
		"merge":     "x-service: &s {image: alpine, env_file: /outside}\nservices:\n  app: {<<: *s}",
		"cycle":     "x-a: &a {a: *a}\nservices: {app: {image: alpine}}",
		"duplicate": "services:\n app: {image: alpine, image: nginx}",
		"unknown":   "services: {app: {image: alpine, future_file: /outside}}",
		"include":   "include: /outside\nservices: {app: {image: alpine}}",
		"tag":       "services: {app: !override {image: alpine}}",
		"disabled":  "services: {app: {image: alpine, profiles: [disabled], env_file: '${SECRET}'}}",
		"provider":  "services: {app: {image: alpine, provider: {type: evil}}}",
		"label":     "services: {app: {image: alpine, label_file: label.txt}}",
		"build":     "services: {app: {image: alpine, build: {context: ., additional_contexts: {x: /outside}}}}",
		"mount":     "services: {app: {image: alpine, volumes: ['${PATH}:/data']}}",
	} {
		t.Run(name, func(t *testing.T) {
			p, e, record := testPackage(t, input, nil, "")
			if _, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{}); err == nil {
				t.Fatal("unsafe input accepted")
			}
			if n := len(calls(t, record)); n != 0 {
				t.Fatalf("unsafe input launched %d CLI calls", n)
			}
		})
	}
}
func TestControlEnvAndLookupRejectedBeforeCLI(t *testing.T) {
	for _, value := range []string{"export COMPOSE_FILE=evil\n", "MULTI='first\nline'\nPATH=evil\n", "SAFE=${DOCKER_HOST}\n"} {
		p, e, record := testPackage(t, "services: {app: {image: alpine}}", map[string]string{".env": value}, "")
		if _, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{}); err == nil {
			t.Fatal("control key accepted")
		}
		if len(calls(t, record)) != 0 {
			t.Fatal("CLI called before env validation")
		}
	}
}

func TestNestedControlVariablesRejectedBeforeCLI(t *testing.T) {
	for _, value := range []string{
		"SAFE=${MISSING-${DOCKER_HOST}}\n",
		"SAFE=${M1_CLI_FIXTURE:+${HOME}}\n",
		"SAFE=${PRESENT:-${MISSING+${PATH}}}\n",
		"SAFE=${UNSET?${COMPOSE_FILE}}\n",
		"SAFE=${UNSET:?${MISSING:-${LD_AUDIT}}}\n",
		"SAFE=prefix-${MISSING:-ok}-${M1_CLI_FIXTURE+${DOCKER_CONFIG}}\n",
		"SAFE=\"first\n${MISSING:-${COMPOSE_PROFILES}}\"\n",
	} {
		p, e, record := testPackage(t, "services: {app: {image: alpine}}", map[string]string{".env": value}, "")
		_, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{})
		if err == nil || len(calls(t, record)) != 0 {
			t.Errorf("potential control lookup reached CLI; err=%v calls=%d", err, len(calls(t, record)))
		}
	}
}
func TestMutatedOperationContextNeverReachesInvocation(t *testing.T) {
	p, e, record := testPackage(t, "services: {app: {image: alpine}}", nil, "")
	p.OperationDir = filepath.Join(t.TempDir(), "unchecked")
	if _, err := e.Invocation(p); err == nil {
		t.Error("unverified operation directory used in CLI context")
	}
	if len(calls(t, record)) != 0 {
		t.Fatal("context rejection launched CLI")
	}
}

func TestEveryOriginalFileIsCheckedBeforeCLI(t *testing.T) {
	p, e, record := testPackage(t, "services: {app: {image: alpine, env_file: /outside}}", map[string]string{"override.yaml": "services: {app: {env_file: []}}"}, "")
	if _, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{}); err == nil {
		t.Fatal("overridden unsafe source accepted")
	}
	if len(calls(t, record)) != 0 {
		t.Fatal("CLI called before all source checks")
	}
}
func TestLiteralDotRelativeFileAccepted(t *testing.T) {
	p, e, _ := testPackage(t, "services: {app: {image: alpine, env_file: ./app.env}}", map[string]string{"app.env": "PATH=/container/path\n"}, "")
	if _, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{}); err != nil {
		t.Fatal(err)
	}
}
func TestExpandedBudgetAndDepth(t *testing.T) {
	input := "x-a: &a [one, two, three, four, five, six, seven, eight, nine, ten]\nx-b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a,*a]\nx-c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b,*b]\nx-d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c,*c]\nx-e: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d,*d]\nservices: {app: {image: alpine}}"
	for _, text := range []string{input, "x-deep: " + strings.Repeat("[", 70) + "x" + strings.Repeat("]", 70) + "\nservices: {app: {image: alpine}}"} {
		p, e, record := testPackage(t, text, nil, "")
		if _, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{}); err == nil || !strings.Contains(err.Error(), "config_complexity_exceeded") {
			t.Fatalf("complex input accepted: %v", err)
		}
		if len(calls(t, record)) != 0 {
			t.Fatal("budget checked after CLI")
		}
	}
}
func TestNormalizedMountRules(t *testing.T) {
	p, _, _ := testPackage(t, "services: {app: {image: alpine}}", map[string]string{"config.txt": "abc"}, "")
	good := Prepared{Package: p, Model: Model{Services: map[string]Service{"app": {Image: "alpine", Volumes: []Mount{{Type: "bind", Source: filepath.Join(p.Dir, "config.txt"), ReadOnly: true}, {Type: "volume", Source: "data"}, {Type: "tmpfs"}}}}}}
	if err := validateModel(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	for _, mount := range []Mount{{Type: "bind", Source: filepath.Join(p.Dir, "config.txt")}, {Type: "bind", Source: p.Dir, ReadOnly: true}, {Type: "bind", Source: filepath.Join(filepath.Dir(p.Dir), "outside"), ReadOnly: true}, {Type: "cluster", Source: "source"}} {
		bad := Prepared{Package: p, Model: Model{Services: map[string]Service{"app": {Image: "alpine", Volumes: []Mount{mount}}}}}
		if validateModel(context.Background(), bad) == nil {
			t.Fatalf("unsafe normalized mount accepted %#v", mount)
		}
	}
}
func TestNativeConfigAndInvocationShareContext(t *testing.T) {
	p, e, record := testPackage(t, "x-service: &s {image: alpine}\nservices: {app: {<<: *s, build: .}}", nil, "")
	prepared, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Evidence.WorkspaceID != p.WorkspaceID || len(prepared.Evidence.Pre.Resources) != 0 {
		t.Fatal("wrong preparation evidence")
	}
	cmd, err := e.Invocation(p)
	if err != nil {
		t.Fatal(err)
	}
	seen := calls(t, record)
	if len(seen) < 4 {
		t.Fatal("native config/list not executed")
	}
	config := seen[0]
	actualEnv := map[string]string{}
	for _, v := range config.Env {
		kv := strings.SplitN(v, "=", 2)
		if len(kv) == 2 {
			actualEnv[kv[0]] = kv[1]
		}
	}
	environmentMatches := true
	for _, v := range cmd.Env {
		kv := strings.SplitN(v, "=", 2)
		if actualEnv[kv[0]] != kv[1] {
			environmentMatches = false
		}
	}
	if !reflect.DeepEqual(config.Args[:len(config.Args)-3], cmd.Args[:len(cmd.Args)-3]) || !environmentMatches {
		t.Fatalf("config/up contexts differ")
	}
	if strings.Join(cmd.Args[len(cmd.Args)-3:], " ") != "up --detach --no-build" {
		t.Fatal("up flags changed")
	}
	for _, v := range cmd.Env {
		if strings.HasPrefix(v, "USERNAME=") || strings.HasPrefix(v, "DOCKER_HOST=") {
			t.Fatal("host env inherited")
		}
	}
	raw, err := workspace.ReadFile(context.Background(), p, "compose.yaml")
	if err != nil || !strings.Contains(string(raw), "<<: *s") {
		t.Fatal("original YAML rewritten")
	}
}
func TestObservationIncludesStoppedAndHealthNone(t *testing.T) {
	p, e, record := testPackage(t, "services: {app: {image: alpine}}", nil, "post")
	prepared := observationInput(p, e, ledger.PreparationEvidence{Pre: ledger.Baseline{Resources: []ledger.ResourceRecord{}}, Expected: []ledger.ExpectedResource{{Kind: "container", LogicalKey: "app"}, {Kind: "network", LogicalKey: "default"}}})
	ev, err := e.Observe(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	var o observation
	if json.Unmarshal(ev.Observation, &o) != nil || o.Status != "complete" || len(o.Items) != 1 || o.Items[0].Health != "none" || o.Items[0].State != "exited" || len(ev.Post.Resources) != 2 || len(ev.Conflicts) != 0 {
		t.Fatalf("invalid observation %s", ev.Observation)
	}
	if o.CollectedAt != ev.Post.CollectedAt {
		t.Fatal("collection timestamps differ")
	}
	seen := calls(t, record)
	allContainers := false
	for _, call := range seen {
		joined := strings.Join(call.Args, " ")
		if strings.Contains(joined, "container ls") && strings.Contains(joined, "--all") {
			allContainers = true
		}
	}
	if !allContainers {
		t.Fatal("stopped resources omitted")
	}
	for _, c := range seen {
		if strings.Contains(strings.Join(c.Args, " "), "remove") {
			t.Fatal("observation mutated resources")
		}
	}
}

func TestUnknownAndNewCandidatesDoNotClaimPublicStack(t *testing.T) {
	for _, expected := range [][]ledger.ExpectedResource{{}, {{Kind: "container", LogicalKey: "app"}, {Kind: "network", LogicalKey: "default"}}} {
		p, e, _ := testPackage(t, "services: {app: {image: alpine}}", nil, "post")
		ev, err := e.Observe(context.Background(), observationInput(p, e, ledger.PreparationEvidence{Pre: ledger.Baseline{Resources: []ledger.ResourceRecord{}}, Expected: expected}))
		if err != nil {
			t.Fatal(err)
		}
		var view observation
		if json.Unmarshal(ev.Observation, &view) != nil || len(view.Items) != 1 {
			t.Fatal("invalid fixture observation")
		}
		if len(expected) == 0 && len(ev.Conflicts) == 0 {
			t.Fatal("unknown fixture lost conflict evidence")
		}
		if view.Items[0].Stack != nil {
			t.Error("unregistered resource publicly claimed request Stack")
		}
	}
}
func TestObservationFailureNeverEmptyComplete(t *testing.T) {
	for _, scenario := range []string{"unreachable", "disappear", "association"} {
		p, e, _ := testPackage(t, "services: {app: {image: alpine}}", nil, scenario)
		ev, err := e.Observe(context.Background(), observationInput(p, e, ledger.PreparationEvidence{}))
		if err == nil {
			t.Fatal("failed observation accepted")
		}
		var o observation
		json.Unmarshal(ev.Observation, &o)
		if o.Status == "complete" || o.Error == nil {
			t.Fatalf("failure became complete: %s", ev.Observation)
		}
	}
}
func TestOwnershipCandidatesRetainSpecificReplacement(t *testing.T) {
	p, e, _ := testPackage(t, "services: {app: {image: alpine}}", nil, "post")
	old := ledger.ResourceRecord{Kind: "container", LogicalKey: "app/1", ResourceRef: strings.Repeat("c", 64), Service: "app", ContainerNumber: 1}
	ev, err := e.Observe(context.Background(), observationInput(p, e, ledger.PreparationEvidence{Pre: ledger.Baseline{Resources: []ledger.ResourceRecord{old}}, Expected: []ledger.ExpectedResource{{Kind: "container", LogicalKey: "app"}, {Kind: "network", LogicalKey: "default"}}}))
	if err != nil || len(ev.Replacements) != 1 || ev.Replacements[0].OldRef != old.ResourceRef || ev.Replacements[0].NewRef != containerID {
		t.Fatalf("replacement missing: %#v %v", ev.Replacements, err)
	}
}
func TestBudgetRejectsWithoutTruncatingSuccess(t *testing.T) {
	if _, err := parseLines([]byte(strings.Repeat("resource\n", 201))); err == nil {
		t.Fatal("budget accepted")
	}
	p, e, _ := testPackage(t, "services: {app: {image: alpine}}", nil, "missingimage")
	if _, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{}); err == nil {
		t.Fatal("missing image accepted")
	}
}

func TestFirstProjectCollisionAndActiveBaseline(t *testing.T) {
	p, e, record := testPackage(t, "services: {app: {image: alpine}}", nil, "post")
	if _, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{}); err == nil || err.Error() != "resource_drift" {
		t.Fatalf("first project imported resources: %v", err)
	}
	observed, err := e.Observe(context.Background(), observationInput(p, e, ledger.PreparationEvidence{}))
	if err != nil {
		t.Fatal(err)
	}
	binding := ledger.ProjectBinding{EndpointID: "endpoint", ProjectName: "demo", StackID: p.Payload.StackID, Status: "active", Revision: 4, Baseline: observed.Post}
	prepared, err := e.Prepare(context.Background(), p, binding)
	if err != nil || prepared.Evidence.OwnershipRevision != 4 {
		t.Fatalf("active baseline mismatch %v", err)
	}
	binding.Baseline.Resources[0].ResourceRef = strings.Repeat("c", 64)
	if _, err = e.Prepare(context.Background(), p, binding); err == nil {
		t.Fatal("external replacement imported")
	}
	for _, c := range calls(t, record) {
		for _, a := range c.Args {
			if a == "up" {
				t.Fatal("preparation started Compose")
			}
		}
	}
}
func TestExternalExistenceAndUnknownNamedVolume(t *testing.T) {
	for _, scenario := range []string{"externalmissing", "externalsuccess", "volumecollision"} {
		p, e, _ := testPackage(t, "services: {app: {image: alpine}}", nil, scenario)
		prepared, err := e.Prepare(context.Background(), p, ledger.ProjectBinding{})
		if scenario == "externalsuccess" {
			if err != nil || len(prepared.Evidence.Pre.Resources) != 0 {
				t.Fatalf("external dependency became owned: %v", err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("unsafe dependency accepted %s", scenario)
		}
	}
}

func TestPublicStackBinding(t *testing.T) {
	for name, mutate := range map[string]func(*ledger.PreparationEvidence){
		"known":         func(*ledger.PreparationEvidence) {},
		"revision_zero": func(ev *ledger.PreparationEvidence) { ev.OwnershipRevision = 0 },
		"replacement":   func(ev *ledger.PreparationEvidence) { ev.Pre.Resources[0].ResourceRef = strings.Repeat("c", 64) },
		"different_service": func(ev *ledger.PreparationEvidence) {
			ev.Pre.Resources[0].Service = "other"
			ev.Pre.Resources[0].LogicalKey = "other/1"
		},
		"different_project": func(ev *ledger.PreparationEvidence) { ev.Pre.Resources[0].ProjectName = "other" },
		"external":          func(ev *ledger.PreparationEvidence) { ev.Pre.Resources[0].Classification = "external" },
		"changed_created":   func(ev *ledger.PreparationEvidence) { ev.Pre.Resources[0].CreatedAt = "2026-11-11T00:00:00Z" },
	} {
		t.Run(name, func(t *testing.T) {
			p, e, _ := testPackage(t, "services: {app: {image: alpine}}", nil, "post")
			pre := ledger.ResourceRecord{Kind: "container", ResourceRef: containerID, LogicalKey: "app/1", Classification: "managed", CreatedAt: "2026-11-12T00:00:00Z", ProjectName: "demo", Service: "app", ContainerNumber: 1}
			partial := ledger.PreparationEvidence{OwnershipRevision: 4, Pre: ledger.Baseline{Resources: []ledger.ResourceRecord{pre}}, Expected: []ledger.ExpectedResource{{Kind: "container", LogicalKey: "app"}, {Kind: "network", LogicalKey: "default"}}}
			mutate(&partial)
			ev, err := e.Observe(context.Background(), observationInput(p, e, partial))
			if err != nil {
				t.Fatal(err)
			}
			var view observation
			json.Unmarshal(ev.Observation, &view)
			if len(view.Items) != 1 {
				t.Fatal("invalid fixture observation")
			}
			if name == "known" {
				if view.Items[0].Stack == nil || *view.Items[0].Stack != p.Payload.StackID {
					t.Fatal("exact registered resource lost association")
				}
			} else if view.Items[0].Stack != nil {
				t.Fatal("nonidentical or unregistered resource claimed Stack")
			}
			if name == "replacement" && len(ev.Replacements) != 1 {
				t.Fatal("replacement private evidence lost")
			}
		})
	}
}

func TestObserveEndpointFence(t *testing.T) {
	for _, scenario := range []string{"enginechanged", "dockerchanged", "versionchanged", "probefailed"} {
		t.Run(scenario, func(t *testing.T) {
			p, e, record := testPackage(t, "services: {app: {image: alpine}}", nil, scenario)
			ev, err := e.Observe(context.Background(), observationInput(p, e, ledger.PreparationEvidence{}))
			var view observation
			json.Unmarshal(ev.Observation, &view)
			if err == nil || view.Status != "error" || view.Error == nil {
				t.Errorf("endpoint change became successful observation: %s", ev.Observation)
			}
			wantCode := "execution_profile_mismatch"
			if scenario == "probefailed" {
				wantCode = "docker_probe_failed"
			}
			if view.Error == nil || view.Error.Code != wantCode {
				t.Error("endpoint failure did not return the explicit diagnosis")
			}
			seen := calls(t, record)
			probeSeen := false
			for _, call := range seen {
				joined := strings.Join(call.Args, " ")
				if strings.Contains(joined, " info --format ") {
					probeSeen = true
				}
				if strings.Contains(joined, " ls ") || strings.Contains(joined, " inspect ") {
					t.Error("collected resources before endpoint was authenticated")
				}
			}
			if !probeSeen {
				t.Error("test did not execute a real Engine probe")
			}
		})
	}
}

func TestObserveEvidenceFence(t *testing.T) {
	for _, field := range []string{"operation", "input", "workspace", "manifest", "profile", "engine", "docker", "compose", "policy", "endpoint"} {
		t.Run(field, func(t *testing.T) {
			p, e, record := testPackage(t, "services: {app: {image: alpine}}", nil, "")
			prepared := observationInput(p, e, ledger.PreparationEvidence{})
			switch field {
			case "operation":
				prepared.Evidence.OperationID = "other"
			case "input":
				prepared.Evidence.InputFingerprint = "other"
			case "workspace":
				prepared.Evidence.WorkspaceID = "other"
			case "manifest":
				prepared.Evidence.ManifestFingerprint = "other"
			case "profile":
				prepared.Evidence.ProfileFingerprint = "other"
			case "engine":
				prepared.Evidence.EngineID = "other"
			case "docker":
				prepared.Evidence.DockerVersion = "other"
			case "compose":
				prepared.Evidence.ComposeVersion = "other"
			case "policy":
				e.profile.Runtime.PolicyRevision++
			case "endpoint":
				e.profile.Runtime.EndpointID = "other"
			}
			ev, err := e.Observe(context.Background(), prepared)
			var view observation
			json.Unmarshal(ev.Observation, &view)
			if err == nil || view.Error == nil || view.Error.Code != "execution_profile_mismatch" {
				t.Error("unbound evidence accepted")
			}
			if len(calls(t, record)) != 0 {
				t.Error("unbound evidence used CLI context")
			}
		})
	}
}
