package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"docker-manager/internal/manager/operations"
)

type errorDefinition struct {
	status                   int
	category, message, retry string
}

var errorDefinitions = map[string]errorDefinition{
	"invalid_json":          {400, "invalid_input", "Invalid JSON document.", "none"},
	"invalid_field":         {400, "invalid_input", "Invalid request field.", "none"},
	"unauthenticated":       {401, "permission_denied", "Authentication required.", "none"},
	"invalid_credentials":   {401, "permission_denied", "Invalid credentials.", "none"},
	"origin_rejected":       {403, "permission_denied", "Request origin rejected.", "none"},
	"csrf_rejected":         {403, "permission_denied", "CSRF token rejected.", "none"},
	"permission_denied":     {403, "permission_denied", "Permission denied.", "none"},
	"not_found":             {404, "not_found", "Object not found.", "none"},
	"idempotency_conflict":  {409, "conflict", "Request key already has different input.", "refresh_and_review"},
	"name_conflict":         {409, "conflict", "Name already exists.", "refresh_and_review"},
	"resource_busy":         {409, "conflict", "Resource is busy.", "refresh_and_review"},
	"recovery_required":     {409, "conflict", "Recovery is required.", "refresh_and_review"},
	"enrollment_active":     {409, "conflict", "Enrollment is already active.", "refresh_and_review"},
	"node_already_enrolled": {409, "conflict", "Node is already enrolled.", "refresh_and_review"},
	"last_admin":            {409, "conflict", "An enabled administrator must remain.", "refresh_and_review"},
	"cursor_expired":        {410, "invalid_input", "Cursor has expired.", "refresh_and_review"},
	"version_conflict":      {412, "conflict", "Version has changed.", "refresh_and_review"},
	"payload_too_large":     {413, "invalid_input", "Request exceeds the size limit.", "none"},
	"unsupported":           {422, "unsupported", "Requested capability is unsupported.", "none"},
	"precondition_required": {428, "invalid_input", "If-Match is required.", "refresh_and_review"},
	"rate_limited":          {429, "unavailable", "Authentication temporarily rate limited.", "same_request"},
	"node_unavailable":      {503, "unavailable", "Node is unavailable.", "same_request"},
	"storage_busy":          {503, "unavailable", "Storage is busy.", "same_request"},
	"storage_full":          {503, "unavailable", "Storage is full.", "same_request"},
	"internal_error":        {500, "unavailable", "Request could not be completed.", "same_request"},
}

func writeError(w http.ResponseWriter, id, code string, retry int) {
	d, ok := errorDefinitions[code]
	if !ok {
		code = "internal_error"
		d = errorDefinitions[code]
	}
	if code == "rate_limited" {
		if retry < 1 {
			retry = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	w.WriteHeader(d.status)
	_ = json.NewEncoder(w).Encode(struct {
		Error any `json:"error"`
	}{struct {
		Category    string                         `json:"category"`
		Code        string                         `json:"code"`
		Message     string                         `json:"message"`
		RequestID   string                         `json:"request_id"`
		FieldErrors []operations.ServiceFieldError `json:"field_errors"`
		RetryAction string                         `json:"retry_action"`
	}{d.category, code, d.message, id, []operations.ServiceFieldError{}, d.retry}})
}
func writeServiceError(w http.ResponseWriter, id string, err error) {
	var se *operations.ServiceError
	if errors.As(err, &se) {
		if d, ok := errorDefinitions[se.Code]; ok && se.Status == d.status {
			writeError(w, id, se.Code, se.RetryAfter)
			return
		}
	}
	writeError(w, id, "internal_error", 0)
}
func authenticationFailure(err error) bool {
	var se *operations.ServiceError
	return errors.As(err, &se) && se.Status == 401
}
