package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
)

type ownershipCandidate struct {
	PreRevision  uint64
	Post         Baseline
	Replacements []Replacement
	Status       string
}

func validResourceKind(k string) bool { return k == "container" || k == "network" || k == "volume" }
func validateBaseline(b Baseline, project string) error {
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", b.CollectedAt); err != nil {
		return ErrEvidence
	}
	keys, refs := map[string]bool{}, map[string]bool{}
	for _, r := range b.Resources {
		k := r.Kind + "\x00" + r.LogicalKey
		ref := r.Kind + "\x00" + r.ResourceRef
		if !validResourceKind(r.Kind) || r.ResourceRef == "" || r.LogicalKey == "" || keys[k] || refs[ref] || (r.Classification != "managed" && r.Classification != "external") {
			return ErrEvidence
		}
		if r.Classification == "managed" && r.ProjectName != project {
			return ErrEvidence
		}
		if r.Kind == "container" && (r.Service == "" || r.ContainerNumber < 1 || r.LogicalKey != fmt.Sprintf("%s/%d", r.Service, r.ContainerNumber)) {
			return ErrEvidence
		}
		if r.Kind != "volume" {
			decoded, err := hex.DecodeString(r.ResourceRef)
			if err != nil || len(decoded) != 32 || r.ResourceRef != hex.EncodeToString(decoded) {
				return ErrEvidence
			}
		}
		if r.Kind == "volume" && r.Driver == "" {
			return ErrEvidence
		}
		keys[k], refs[ref] = true, true
	}
	return nil
}
func managedResources(b Baseline) []ResourceRecord {
	result := []ResourceRecord{}
	for _, r := range b.Resources {
		if r.Classification == "managed" {
			result = append(result, r)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Kind+"/"+result[i].LogicalKey < result[j].Kind+"/"+result[j].LogicalKey
	})
	return result
}
func sameResources(a, b Baseline) bool {
	return reflect.DeepEqual(managedResources(a), managedResources(b))
}

// readProjectBinding authenticates both publication and current status in the
// caller's snapshot. No decrypted candidate alone can grant active ownership.
func (s *Store) readProjectBinding(ctx context.Context, q querier, endpoint, project string) (ProjectBinding, error) {
	b := ProjectBinding{EndpointID: endpoint, ProjectName: project, Baseline: Baseline{Resources: []ResourceRecord{}}}
	var op sql.NullString
	var seq sql.NullInt64
	err := q.QueryRowContext(ctx, "SELECT stack_id,ownership_revision,status,last_operation_id,baseline_seq FROM project_bindings WHERE endpoint_id=? AND project_name=?", endpoint, project).Scan(&b.StackID, &b.Revision, &b.Status, &op, &seq)
	if err == sql.ErrNoRows {
		return b, ErrNotFound
	}
	if err != nil {
		return b, ErrStorage
	}
	if op.Valid != seq.Valid || (op.Valid && (seq.Int64 < 1 || b.Revision == 0 || b.Status == "provisional")) || (!op.Valid && (b.Revision != 0 || b.Status == "active")) {
		return b, ErrEvidence
	}
	head, err := s.readOwnershipPublicationHead(ctx, q, b)
	if err != nil {
		return b, err
	}
	expectedStatus := "provisional"
	if head != nil {
		if !op.Valid || op.String != head.record.OperationID || uint64(seq.Int64) != head.seq || b.Revision != head.revision {
			return b, ErrEvidence
		}
		b.Baseline, b.LastOperationID = head.candidate.Post, head.record.OperationID
		expectedStatus = "active"
	} else if op.Valid {
		return b, ErrEvidence
	}
	drifted, err := s.hasUnreleasedOwnershipDrift(ctx, q, b)
	if err != nil {
		return b, err
	}
	if drifted {
		expectedStatus = "drifted"
	}
	if b.Status != expectedStatus {
		return b, ErrEvidence
	}
	return b, nil
}

type ownershipPublication struct {
	record        Record
	seq, revision uint64
	candidate     ownershipCandidate
}

// The authenticated histories are the authority for the head: replaying an old
// valid pointer plus its old revision must not roll back a newer publication.
// All cursors are closed before decrypting on the Store's sole connection.
func (s *Store) readOwnershipPublicationHead(ctx context.Context, q querier, b ProjectBinding) (*ownershipPublication, error) {
	rows, err := q.QueryContext(ctx, "SELECT x.operation_id,f.result_event_seq FROM executions x JOIN finalizations f ON f.operation_id=x.operation_id JOIN runtime_documents d ON d.operation_id=f.operation_id AND d.kind='candidate' AND d.seq=f.result_event_seq WHERE x.docker_endpoint_id=? AND x.project_name=?", b.EndpointID, b.ProjectName)
	if err != nil {
		return nil, ErrStorage
	}
	refs := []struct {
		operation string
		seq       uint64
	}{}
	for rows.Next() {
		var ref struct {
			operation string
			seq       uint64
		}
		if rows.Scan(&ref.operation, &ref.seq) != nil {
			rows.Close()
			return nil, ErrStorage
		}
		refs = append(refs, ref)
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil || closeErr != nil {
		return nil, ErrStorage
	}
	byRevision := map[uint64]*ownershipPublication{}
	var head *ownershipPublication
	for _, ref := range refs {
		r, err := s.readRecord(ctx, q, ref.operation)
		if err != nil {
			return nil, bindingEvidenceError(err)
		}
		state, err := readEventState(ctx, q, r.OperationID)
		if err != nil {
			return nil, bindingEvidenceError(err)
		}
		if state.Blocked {
			return nil, ErrEvidence
		}
		view, err := s.readRecoveryRecord(ctx, q, r.OperationID)
		if err != nil {
			return nil, bindingEvidenceError(err)
		}
		var finalized struct {
			Phase string
			Seq   *uint64 `json:"finalized_result_event_seq"`
		}
		if json.Unmarshal(view, &finalized) != nil || finalized.Phase != "finalized" || finalized.Seq == nil || *finalized.Seq != ref.seq {
			return nil, ErrEvidence
		}
		var c ownershipCandidate
		if err = s.readDocument(ctx, q, r, "candidate", ref.seq, &c); err != nil {
			return nil, bindingEvidenceError(err)
		}
		if r.Phase != PhaseStartCommitted || c.Status != "consistent" || c.PreRevision >= 9007199254740991 || r.StackID != b.StackID || r.EndpointID != b.EndpointID || r.ProjectName != b.ProjectName {
			return nil, ErrEvidence
		}
		if err = validateBaseline(c.Post, b.ProjectName); err != nil {
			return nil, ErrEvidence
		}
		publication := &ownershipPublication{record: r, seq: ref.seq, revision: c.PreRevision + 1, candidate: c}
		if _, exists := byRevision[publication.revision]; exists {
			return nil, ErrEvidence
		}
		byRevision[publication.revision] = publication
		if head == nil || publication.revision > head.revision {
			head = publication
		}
	}
	if head != nil {
		if head.revision != uint64(len(byRevision)) {
			return nil, ErrEvidence
		}
		for revision := uint64(1); revision <= head.revision; revision++ {
			if byRevision[revision] == nil {
				return nil, ErrEvidence
			}
		}
	}
	return head, nil
}

// Only a still-held operation's authenticated latest candidate can establish
// drift. Plain status changes cannot erase a known drift or invent a new one.
func (s *Store) hasUnreleasedOwnershipDrift(ctx context.Context, q querier, b ProjectBinding) (bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT operation_id FROM executions x WHERE docker_endpoint_id=? AND project_name=? AND stack_id=? AND NOT EXISTS (SELECT 1 FROM finalizations f WHERE f.operation_id=x.operation_id)", b.EndpointID, b.ProjectName, b.StackID)
	if err != nil {
		return false, ErrStorage
	}
	operations := []string{}
	for rows.Next() {
		var operation string
		if rows.Scan(&operation) != nil {
			rows.Close()
			return false, ErrStorage
		}
		operations = append(operations, operation)
	}
	rowErr := rows.Err()
	closeErr := rows.Close()
	if rowErr != nil || closeErr != nil {
		return false, ErrStorage
	}
	drifted := false
	for _, operation := range operations {
		r, err := s.readRecord(ctx, q, operation)
		if err != nil {
			return false, bindingEvidenceError(err)
		}
		if err = requireEventClaims(ctx, q, r); err != nil {
			return false, bindingEvidenceError(err)
		}
		state, err := readEventState(ctx, q, operation)
		if err != nil {
			return false, bindingEvidenceError(err)
		}
		if state.Blocked {
			return false, ErrEvidence
		}
		if _, err = s.readRecoveryRecord(ctx, q, r.OperationID); err != nil {
			return false, bindingEvidenceError(err)
		}
		if !state.LatestResultSeq.Valid {
			continue
		}
		event, err := s.readEvent(ctx, q, operation, uint64(state.LatestResultSeq.Int64))
		if err != nil {
			return false, bindingEvidenceError(err)
		}
		var result runtimeResult
		if json.Unmarshal(event.Body, &result) != nil || event.Kind != "result" {
			return false, ErrEvidence
		}
		if result.Outcome == "not_started" {
			continue
		}
		var c ownershipCandidate
		if err = s.readDocument(ctx, q, r, "candidate", event.Seq, &c); err != nil {
			return false, bindingEvidenceError(err)
		}
		if c.Status != result.Ownership {
			return false, ErrEvidence
		}
		if c.Status == "drifted" {
			drifted = true
		}
	}
	return drifted, nil
}

func (s *Store) ReadProjectBinding(ctx context.Context, endpoint, project string) (ProjectBinding, error) {
	if err := s.requireRuntime(); err != nil {
		return ProjectBinding{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectBinding{}, ErrStorage
	}
	defer tx.Rollback()
	return s.readProjectBinding(ctx, tx, endpoint, project)
}
func (s *Store) ownership(ctx context.Context, q querier, r Record, e ObservationEvidence) (ownershipCandidate, error) {
	var prep PreparationEvidence
	if err := s.readDocument(ctx, q, r, "preparation", 0, &prep); err != nil {
		return ownershipCandidate{}, err
	}
	c := ownershipCandidate{PreRevision: prep.OwnershipRevision, Post: e.Post, Replacements: e.Replacements, Status: "unknown"}
	var obs struct {
		Status      string `json:"status"`
		CollectedAt string `json:"collected_at"`
		Items       []struct {
			Kind    string  `json:"resource_kind"`
			ID      string  `json:"resource_id"`
			Project *string `json:"project_name"`
			Service *string `json:"service_name"`
			Stack   *string `json:"stack_id"`
		} `json:"items"`
	}
	if json.Unmarshal(e.Observation, &obs) != nil {
		return c, ErrEvidence
	}
	if len(e.Conflicts) > 0 {
		c.Status = "drifted"
		return c, nil
	}
	if obs.Status != "complete" {
		return c, nil
	}
	if obs.CollectedAt != e.Post.CollectedAt {
		return c, ErrEvidence
	}
	if err := validateBaseline(e.Post, r.ProjectName); err != nil {
		return c, err
	}
	binding, err := s.readProjectBinding(ctx, q, r.EndpointID, r.ProjectName)
	if err != nil {
		return c, err
	}
	if binding.StackID != r.StackID || binding.Revision != prep.OwnershipRevision || binding.Status == "drifted" || !sameResources(binding.Baseline, prep.Pre) {
		c.Status = "drifted"
		return c, nil
	}
	expected := map[string]bool{}
	for _, x := range prep.Expected {
		if !x.External {
			expected[x.Kind+"\x00"+x.LogicalKey] = true
		}
	}
	before := map[string]ResourceRecord{}
	for _, x := range managedResources(prep.Pre) {
		before[x.Kind+"\x00"+x.LogicalKey] = x
	}
	post := map[string]ResourceRecord{}
	containers := map[string]ResourceRecord{}
	calculated := []Replacement{}
	for _, x := range managedResources(e.Post) {
		k := x.Kind + "\x00" + x.LogicalKey
		post[k] = x
		if x.Kind == "container" {
			containers[x.ResourceRef] = x
		}
		old, ok := before[k]
		if ok && old == x {
			continue
		}
		if !expected[k] && !(x.Kind == "container" && expected["container\x00"+x.Service]) {
			c.Status = "drifted"
			return c, nil
		}
		if ok {
			// An identity retained with changed associations is external drift, not a
			// legitimate replacement. The collector must explicitly name every swap.
			if old.ResourceRef == x.ResourceRef {
				c.Status = "drifted"
				return c, nil
			}
			calculated = append(calculated, Replacement{x.Kind, x.LogicalKey, old.ResourceRef, x.ResourceRef})
		}
	}
	for k := range before {
		if _, ok := post[k]; !ok {
			c.Status = "drifted"
			return c, nil
		}
	}
	less := func(a, b Replacement) bool { return a.Kind+"/"+a.LogicalKey < b.Kind+"/"+b.LogicalKey }
	sort.Slice(calculated, func(i, j int) bool { return less(calculated[i], calculated[j]) })
	supplied := append([]Replacement{}, e.Replacements...)
	sort.Slice(supplied, func(i, j int) bool { return less(supplied[i], supplied[j]) })
	if !reflect.DeepEqual(calculated, supplied) {
		c.Status = "drifted"
		return c, nil
	}
	if len(obs.Items) != len(containers) {
		return c, ErrEvidence
	}
	for _, item := range obs.Items {
		x, ok := containers[item.ID]
		if !ok || item.Kind != "container" || item.Project == nil || *item.Project != r.ProjectName || item.Service == nil || *item.Service != x.Service || (item.Stack != nil && *item.Stack != r.StackID) {
			return c, ErrEvidence
		}
		delete(containers, item.ID)
	}
	for _, x := range managedResources(e.Post) {
		if x.Kind == "volume" && x.CreatedAt == "" {
			return c, nil
		}
	}
	c.Status = "consistent"
	return c, nil
}
func (s *Store) publishOwnership(ctx context.Context, tx *sql.Tx, r Record, seq uint64, body []byte) error {
	var result struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(body, &result) != nil {
		return ErrEvidence
	}
	if result.Outcome == "not_started" {
		return nil
	}
	var c ownershipCandidate
	if err := s.readDocument(ctx, tx, r, "candidate", seq, &c); err != nil {
		return err
	}
	if c.Status != "consistent" {
		return ErrConflict
	}
	b, err := s.readProjectBinding(ctx, tx, r.EndpointID, r.ProjectName)
	if err != nil {
		return err
	}
	if b.StackID != r.StackID || b.Revision != c.PreRevision || b.Status == "drifted" {
		return ErrConflict
	}
	changed, err := tx.ExecContext(ctx, `UPDATE project_bindings SET ownership_revision=ownership_revision+1,status='active',last_operation_id=?,baseline_seq=? WHERE endpoint_id=? AND project_name=? AND ownership_revision=?`, r.OperationID, seq, r.EndpointID, r.ProjectName, c.PreRevision)
	if err != nil {
		return ErrStorage
	}
	n, err := changed.RowsAffected()
	if err != nil || n != 1 {
		return ErrConflict
	}
	return nil
}

// Missing referents of an existing binding are damaged evidence, not an absent
// project. Storage errors keep their classification so callers can retry IO.
func bindingEvidenceError(err error) error {
	if err == ErrStorage {
		return err
	}
	return ErrEvidence
}
