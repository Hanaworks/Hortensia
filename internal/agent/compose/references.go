package compose

import (
	"bytes"
	"context"
	"docker-manager/internal/agent/workspace"
	"fmt"
	"github.com/compose-spec/compose-go/v2/schema"
	"go.yaml.in/yaml/v3"
	"io"
	"path"
	"strings"
)

type Diagnostic struct {
	Code, File, Field string
	Line, Column      int
}

func (d *Diagnostic) Error() string {
	return fmt.Sprintf("%s: %s %s (%d:%d)", d.Code, d.File, d.Field, d.Line, d.Column)
}
func diag(code, file, field string, n *yaml.Node) error {
	d := &Diagnostic{Code: code, File: file, Field: field}
	if n != nil {
		d.Line = n.Line
		d.Column = n.Column
	}
	return d
}

type astBudget struct {
	nodes  int
	active map[*yaml.Node]bool
	file   string
}

func (b *astBudget) expand(n *yaml.Node, depth int) (any, error) {
	b.nodes++
	if depth > 64 || b.nodes > 100000 {
		return nil, diag("config_complexity_exceeded", b.file, "$", n)
	}
	if b.active[n] {
		return nil, diag("yaml_alias_cycle", b.file, "$", n)
	}
	b.active[n] = true
	defer delete(b.active, n)
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, diag("yaml_invalid", b.file, "$", n)
		}
		return b.expand(n.Content[0], depth+1)
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, diag("yaml_invalid", b.file, "$", n)
		}
		return b.expand(n.Alias, depth+1)
	case yaml.MappingNode:
		if n.Tag != "!!map" {
			return nil, diag("yaml_tag_unsupported", b.file, "$", n)
		}
		out := map[string]any{}
		explicit := map[string]any{}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			b.nodes++
			if b.nodes > 100000 {
				return nil, diag("config_complexity_exceeded", b.file, "$", k)
			}
			if k.Kind != yaml.ScalarNode || (k.Tag != "!!str" && k.Tag != "!!merge") {
				return nil, diag("yaml_key_invalid", b.file, "$", k)
			}
			if seen[k.Value] {
				return nil, diag("yaml_duplicate_key", b.file, k.Value, k)
			}
			seen[k.Value] = true
			v, err := b.expand(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			if k.Tag == "!!merge" {
				var maps []any
				if seq, ok := v.([]any); ok {
					maps = seq
				} else {
					maps = []any{v}
				}
				for _, m := range maps {
					mm, ok := m.(map[string]any)
					if !ok {
						return nil, diag("yaml_merge_invalid", b.file, "$", k)
					}
					for name, value := range mm {
						if _, ok = out[name]; !ok {
							out[name] = value
						}
					}
				}
			} else {
				explicit[k.Value] = v
			}
		}
		for k, v := range explicit {
			out[k] = v
		}
		return out, nil
	case yaml.SequenceNode:
		if n.Tag != "!!seq" {
			return nil, diag("yaml_tag_unsupported", b.file, "$", n)
		}
		out := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			v, err := b.expand(child, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str", "!!bool", "!!int", "!!float", "!!null":
		default:
			return nil, diag("yaml_tag_unsupported", b.file, "$", n)
		}
		var v any
		if err := n.Decode(&v); err != nil {
			return nil, diag("yaml_invalid", b.file, "$", n)
		}
		return v, nil
	default:
		return nil, diag("yaml_invalid", b.file, "$", n)
	}
}
func controlKey(k string) bool {
	k = strings.ToUpper(k)
	if strings.HasPrefix(k, "DOCKER_") || strings.HasPrefix(k, "COMPOSE_") || strings.HasPrefix(k, "LD_") || strings.HasPrefix(k, "DYLD_") || strings.HasPrefix(k, "LC_") {
		return true
	}
	switch k {
	case "PATH", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "SHELL", "ENV", "BASH_ENV", "CDPATH", "IFS", "TMP", "TEMP", "TMPDIR", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "SYSTEMROOT", "WINDIR", "PATHEXT", "LANG", "LANGUAGE", "GODEBUG", "GOTRACEBACK":
		return true
	}
	return false
}
func literal(s string) bool {
	return s != "" && !strings.ContainsAny(s, "$~\x00") && !strings.Contains(s, "://")
}
func reference(ctx context.Context, p workspace.Package, file, field string, v any) error {
	s, ok := v.(string)
	clean := path.Clean(s)
	if !ok || !literal(s) || strings.ContainsAny(s, "\\:\x00") || !workspace.SafePath(clean) {
		return diag("unsupported_file_reference", file, field, nil)
	}
	if _, err := workspace.ReadFile(ctx, p, clean); err != nil {
		return diag("unsupported_file_reference", file, field, nil)
	}
	return nil
}
func fileList(ctx context.Context, p workspace.Package, file, field string, v any) error {
	if seq, ok := v.([]any); ok {
		for _, x := range seq {
			if err := reference(ctx, p, file, field, x); err != nil {
				return err
			}
		}
		return nil
	}
	return reference(ctx, p, file, field, v)
}
func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func stringsAny(v any) []any     { a, _ := v.([]any); return a }

func staticCheck(ctx context.Context, p workspace.Package, allowLabels bool) (absoluteResult map[string]bool, finalError error) {
	// Resolve a rejected inherited reference back to the original key node,
	// including keys inside an anchor. No substituted YAML is emitted.
	sources := map[string]*yaml.Node{}
	defer func() {
		d, ok := finalError.(*Diagnostic)
		if !ok || d.Line > 0 {
			return
		}
		key := d.Field
		if at := strings.LastIndex(key, "."); at >= 0 {
			key = key[at+1:]
		}
		var find func(*yaml.Node) *yaml.Node
		find = func(n *yaml.Node) *yaml.Node {
			if n.Kind == yaml.MappingNode {
				for i := 0; i < len(n.Content); i += 2 {
					if n.Content[i].Value == key {
						return n.Content[i]
					}
				}
			}
			for _, child := range n.Content {
				if found := find(child); found != nil {
					return found
				}
			}
			return nil
		}
		if n := sources[d.File]; n != nil {
			if found := find(n); found != nil {
				d.Line = found.Line
				d.Column = found.Column
				return
			}
		}
		d.Line = 1
		d.Column = 1
	}()
	if err := workspace.Verify(ctx, p); err != nil {
		return nil, err
	}
	absoluteBinds := map[string]bool{}
	for k := range p.Payload.Config.Env {
		if controlKey(k) {
			return nil, diag("execution_environment_key_forbidden", "payload", "config.interpolation_env", nil)
		}
	}
	envFiles := p.Payload.Config.EnvFiles
	if len(envFiles) == 0 {
		for _, n := range workspace.Names(p) {
			if n == ".env" {
				envFiles = []string{n}
			}
		}
	}
	for _, name := range envFiles {
		raw, err := workspace.ReadFile(ctx, p, name)
		if err != nil {
			return nil, err
		}
		if err = validateInterpolationEnv(raw); err != nil {
			return nil, diag(err.Error(), name, "$", nil)
		}
	}
	for _, file := range p.Payload.Config.ComposeFiles {
		raw, err := workspace.ReadFile(ctx, p, file)
		if err != nil {
			return nil, err
		}
		d := yaml.NewDecoder(bytes.NewReader(raw))
		var node yaml.Node
		if d.Decode(&node) != nil {
			return nil, diag("yaml_invalid", file, "$", nil)
		}
		sources[file] = &node
		var extra yaml.Node
		if d.Decode(&extra) != io.EOF {
			return nil, diag("yaml_multiple_documents", file, "$", nil)
		}
		budget := astBudget{active: map[*yaml.Node]bool{}, file: file}
		v, err := budget.expand(&node, 0)
		if err != nil {
			return nil, err
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, diag("yaml_invalid", file, "$", &node)
		}
		for _, k := range []string{"include", "models"} {
			if _, ok = m[k]; ok {
				return nil, diag("unsupported_file_reference", file, k, &node)
			}
		}
		if err = schema.Validate(m); err != nil {
			return nil, diag("compose_schema_invalid", file, "$", &node)
		}
		for _, kind := range []string{"configs", "secrets"} {
			for key, value := range asMap(m[kind]) {
				item := asMap(value)
				field := kind + "." + key
				if item == nil {
					return nil, diag("unsupported_file_reference", file, field, &node)
				}
				for _, k := range []string{"external", "environment", "content"} {
					if _, ok = item[k]; ok {
						return nil, diag("unsupported_file_reference", file, field+"."+k, &node)
					}
				}
				if err = reference(ctx, p, file, field+".file", item["file"]); err != nil {
					return nil, err
				}
			}
		}
		for service, value := range asMap(m["services"]) {
			s := asMap(value)
			if s == nil {
				continue
			}
			field := "services." + service
			for _, k := range []string{"extends", "credential_spec", "provider", "develop", "pre_start", "post_start", "pre_stop", "models"} {
				if _, ok = s[k]; ok {
					return nil, diag("unsupported_file_reference", file, field+"."+k, &node)
				}
			}
			for _, k := range []string{"env_file", "label_file"} {
				if value, ok = s[k]; ok {
					if k == "label_file" && !allowLabels {
						return nil, diag("unsupported_file_reference", file, field+"."+k, &node)
					}
					if err = fileList(ctx, p, file, field+"."+k, value); err != nil {
						return nil, err
					}
				}
			}
			if value, ok = s["build"]; ok {
				switch b := value.(type) {
				case string:
					if b != "." {
						return nil, diag("unsupported_file_reference", file, field+".build", &node)
					}
				case map[string]any:
					for k := range b {
						switch k {
						case "context", "dockerfile", "args", "target", "labels", "no_cache", "pull", "platforms":
						default:
							return nil, diag("unsupported_file_reference", file, field+".build."+k, &node)
						}
					}
					if c, ok := b["context"]; ok && c != "." {
						return nil, diag("unsupported_file_reference", file, field+".build.context", &node)
					}
					if f, ok := b["dockerfile"]; ok {
						if err = reference(ctx, p, file, field+".build.dockerfile", f); err != nil {
							return nil, err
						}
					}
				default:
					return nil, diag("unsupported_file_reference", file, field+".build", &node)
				}
			}
			for _, mount := range stringsAny(s["volumes"]) {
				switch x := mount.(type) {
				case string:
					if !literal(x) {
						return nil, diag("unsupported_file_reference", file, field+".volumes", &node)
					}
					if strings.HasPrefix(x, "/") {
						absoluteBinds[x] = true
					}
				case map[string]any:
					if source, ok := x["source"]; ok {
						src, ok := source.(string)
						if !ok || !literal(src) {
							return nil, diag("unsupported_file_reference", file, field+".volumes.source", &node)
						}
						if strings.HasPrefix(src, "/") {
							absoluteBinds[src] = true
						}
					}
				}
			}
		}
	}
	return absoluteBinds, nil
}
