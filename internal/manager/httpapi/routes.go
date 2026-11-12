package httpapi

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"docker-manager/internal/manager/operations"
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var decimalPattern = regexp.MustCompile(`^[0-9]+$`)
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

type route struct {
	method, path, kind string
	empty, list, key   bool
	condition          string
}

// Placeholders are validated only after the complete template matches. The
// request URL is never cleaned, redirected, or used as a filesystem path.
var routes = []route{
	{"POST", "/api/v1/sessions", "login", false, false, false, ""},
	{"DELETE", "/api/v1/sessions/current", "logout", true, false, false, ""},
	{"GET", "/api/v1/me", "me", true, false, false, ""},
	{"POST", "/api/v1/users", "service", false, false, true, ""},
	{"GET", "/api/v1/users", "service", true, true, false, ""},
	{"GET", "/api/v1/users/{id}", "service", true, false, false, ""},
	{"PATCH", "/api/v1/users/{id}", "service", false, false, true, "revision"},
	{"POST", "/api/v1/nodes", "service", false, false, true, ""},
	{"GET", "/api/v1/nodes", "service", true, true, false, ""},
	{"GET", "/api/v1/nodes/{id}", "service", true, false, false, ""},
	{"GET", "/api/v1/nodes/{id}/grants", "service", true, true, false, ""},
	{"PUT", "/api/v1/nodes/{id}/grants/{id}", "service", false, false, true, "revision"},
	{"POST", "/api/v1/nodes/{id}/enrollment-tokens", "service", false, false, true, ""},
	{"POST", "/api/v1/nodes/{id}/revoke", "service", true, false, true, ""},
	{"GET", "/api/v1/nodes/{id}/stacks", "service", true, true, false, ""},
	{"POST", "/api/v1/nodes/{id}/stacks", "service", false, false, true, ""},
	{"GET", "/api/v1/stacks/{id}", "service", true, false, false, ""},
	{"GET", "/api/v1/stacks/{id}/versions/{id}", "service", true, false, false, ""},
	{"POST", "/api/v1/stacks/{id}/versions", "service", false, false, true, "version"},
	{"POST", "/api/v1/stacks/{id}/apply", "service", false, false, true, ""},
	{"GET", "/api/v1/operations/{id}", "service", true, false, false, ""},
	{"GET", "/api/v1/operations", "operations", true, true, false, ""},
	{"GET", "/api/v1/submissions/{id}", "service", true, false, false, ""},
	{"POST", "/api/v1/operations/{id}/cancel", "service", true, false, false, ""},
	{"GET", "/api/v1/operations/{id}/output", "output", true, false, false, ""},
	{"POST", "/agent/v1/enroll", "enroll", false, false, false, ""},
	{"POST", "/agent/v1/credentials/rotate", "rotate", false, false, false, ""},
}

func classify(r *http.Request) (route, string) {
	if r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path || strings.Contains(r.URL.Path, "//") || strings.HasSuffix(r.URL.Path, "/") {
		return route{}, "not_found"
	}
	parts := strings.Split(r.URL.Path, "/")
	for _, rt := range routes {
		if r.Method != rt.method {
			continue
		}
		template := strings.Split(rt.path, "/")
		if len(parts) != len(template) {
			continue
		}
		match := true
		badID := false
		for i, part := range template {
			if part == "{id}" {
				badID = badID || !idPattern.MatchString(parts[i])
			} else if part != parts[i] {
				match = false
				break
			}
		}
		if match {
			if badID {
				return route{}, "invalid_field"
			}
			return rt, ""
		}
	}
	return route{}, "not_found"
}

func singleHeader(r *http.Request, name string) (string, bool) {
	v := r.Header.Values(name)
	returnValue := ""
	if len(v) == 1 {
		returnValue = v[0]
	}
	return returnValue, len(v) == 1
}

func parseRequest(r *http.Request, rt route) (operations.ServiceRequest, string) {
	sr := operations.ServiceRequest{Method: r.Method, Path: r.URL.Path}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return sr, "invalid_field"
	}
	for k, v := range q {
		if len(v) != 1 {
			return sr, "invalid_field"
		}
		switch k {
		case "limit":
			if !rt.list && rt.kind != "output" || !decimalPattern.MatchString(v[0]) {
				return sr, "invalid_field"
			}
			n, e := strconv.Atoi(v[0])
			max := 200
			if rt.kind == "output" {
				max = 262144
			}
			if e != nil || n < 1 || n > max {
				return sr, "invalid_field"
			}
			sr.Limit = n
		case "cursor":
			if !rt.list || len(v[0]) < 1 || len(v[0]) > 1024 {
				return sr, "invalid_field"
			}
			sr.Cursor = v[0]
		case "node_id":
			if rt.kind != "operations" || !idPattern.MatchString(v[0]) {
				return sr, "invalid_field"
			}
			sr.NodeID = v[0]
		case "after":
			if rt.kind != "output" || !decimalPattern.MatchString(v[0]) {
				return sr, "invalid_field"
			}
			if _, e := strconv.ParseUint(v[0], 10, 64); e != nil {
				return sr, "invalid_field"
			}
			sr.After = v[0]
		default:
			return sr, "invalid_field"
		}
	}
	if rt.kind == "operations" && sr.NodeID == "" {
		return sr, "invalid_field"
	}
	if rt.key {
		v, ok := singleHeader(r, "Idempotency-Key")
		if !ok || !idPattern.MatchString(v) {
			return sr, "invalid_field"
		}
		sr.IdempotencyKey = v
	}
	if rt.condition != "" {
		v, ok := singleHeader(r, "If-Match")
		if len(r.Header.Values("If-Match")) == 0 {
			return sr, "precondition_required"
		}
		if !ok || len(v) < 3 || v[0] != '"' || v[len(v)-1] != '"' {
			return sr, "invalid_field"
		}
		inner := v[1 : len(v)-1]
		if rt.condition == "version" {
			if !idPattern.MatchString(inner) {
				return sr, "invalid_field"
			}
		} else {
			n, e := strconv.ParseUint(inner, 10, 64)
			if !decimalPattern.MatchString(inner) || e != nil || n > 9007199254740991 || strconv.FormatUint(n, 10) != inner {
				return sr, "invalid_field"
			}
		}
		sr.IfMatch = v
	}
	return sr, ""
}
