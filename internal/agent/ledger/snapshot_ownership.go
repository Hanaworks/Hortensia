package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"sort"
)

func inspectConsistentOwnership(r Record, prep PreparationEvidence, c ownershipCandidate, raw json.RawMessage) error {
	var obs struct {
		Status    string `json:"status"`
		Collected string `json:"collected_at"`
		Items     []struct {
			Kind    string  `json:"resource_kind"`
			ID      string  `json:"resource_id"`
			Project *string `json:"project_name"`
			Service *string `json:"service_name"`
			Stack   *string `json:"stack_id"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &obs) != nil || obs.Status != "complete" || obs.Collected != c.Post.CollectedAt || validateBaseline(c.Post, r.ProjectName) != nil {
		return ErrEvidence
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
	for _, x := range managedResources(c.Post) {
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
			return ErrEvidence
		}
		if ok {
			if old.ResourceRef == x.ResourceRef {
				return ErrEvidence
			}
			calculated = append(calculated, Replacement{x.Kind, x.LogicalKey, old.ResourceRef, x.ResourceRef})
		}
	}
	for k := range before {
		if _, ok := post[k]; !ok {
			return ErrEvidence
		}
	}
	less := func(a, b Replacement) bool { return a.Kind+"/"+a.LogicalKey < b.Kind+"/"+b.LogicalKey }
	sort.Slice(calculated, func(i, j int) bool { return less(calculated[i], calculated[j]) })
	supplied := append([]Replacement{}, c.Replacements...)
	sort.Slice(supplied, func(i, j int) bool { return less(supplied[i], supplied[j]) })
	if !reflect.DeepEqual(calculated, supplied) || len(obs.Items) != len(containers) {
		return ErrEvidence
	}
	for _, item := range obs.Items {
		x, ok := containers[item.ID]
		if !ok || item.Kind != "container" || item.Project == nil || *item.Project != r.ProjectName || item.Service == nil || *item.Service != x.Service || (item.Stack != nil && *item.Stack != r.StackID) {
			return ErrEvidence
		}
		delete(containers, item.ID)
	}
	for _, x := range managedResources(c.Post) {
		if x.Kind == "volume" && x.CreatedAt == "" {
			return ErrEvidence
		}
	}
	return nil
}

func (s *Store) inspectPublications(ctx context.Context, tx *sql.Tx, endpoint, project string, visit func(ownershipPublication) error) error {
	after := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT x.operation_id FROM executions x JOIN finalizations f ON f.operation_id=x.operation_id JOIN runtime_documents d ON d.operation_id=f.operation_id AND d.kind='candidate' AND d.seq=f.result_event_seq WHERE x.docker_endpoint_id=? AND x.project_name=? AND x.operation_id>? ORDER BY x.operation_id LIMIT 64`, endpoint, project, after)
		if err != nil {
			return ErrStorage
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if rows.Scan(&id) != nil {
				rows.Close()
				return ErrStorage
			}
			ids = append(ids, id)
		}
		e, c := rows.Err(), rows.Close()
		if e != nil || c != nil {
			return ErrStorage
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = inspectionExpect(ctx, tx, 0, `SELECT count(*) FROM local_claims WHERE operation_id=?`, id); err != nil {
				return err
			}
			r, e := s.readRecord(ctx, tx, id)
			if e != nil {
				return e
			}
			state, e := readEventState(ctx, tx, id)
			if e != nil {
				return e
			}
			raw, f, e := s.readFinalization(ctx, tx, r, state)
			clear(raw)
			if e != nil {
				return e
			}
			var candidate ownershipCandidate
			if e = s.readDocument(ctx, tx, r, "candidate", f.Seq, &candidate); e != nil {
				return e
			}
			if candidate.Status != "consistent" || candidate.PreRevision >= 9007199254740991 || r.Phase != PhaseStartCommitted || validateBaseline(candidate.Post, r.ProjectName) != nil {
				return ErrEvidence
			}
			if e = visit(ownershipPublication{record: r, seq: f.Seq, revision: candidate.PreRevision + 1, candidate: candidate}); e != nil {
				return e
			}
		}
		after = ids[len(ids)-1]
	}
}

func (s *Store) inspectPublicationAt(ctx context.Context, tx *sql.Tx, endpoint, project string, revision uint64) (ownershipPublication, error) {
	var found ownershipPublication
	count := 0
	err := s.inspectPublications(ctx, tx, endpoint, project, func(p ownershipPublication) error {
		if p.revision == revision {
			count++
			found = p
		}
		return nil
	})
	if err != nil {
		return found, err
	}
	if count != 1 {
		return found, ErrEvidence
	}
	return found, nil
}

func (s *Store) inspectBindings(ctx context.Context, tx *sql.Tx) error {
	cols, err := inspectionColumns(ctx, tx, "project_bindings")
	if err != nil {
		return err
	}
	return inspectionWalk(ctx, tx, "project_bindings", cols, func(v []any) error {
		endpoint, project, stack := v[0].(string), v[1].(string), v[2].(string)
		revision := uint64(v[3].(int64))
		status := v[4].(string)
		if !validID(endpoint) || !validID(stack) {
			return ErrEvidence
		}
		var head ownershipPublication
		var count, max uint64
		if e := s.inspectPublications(ctx, tx, endpoint, project, func(p ownershipPublication) error {
			if p.record.StackID != stack {
				return ErrEvidence
			}
			count++
			if p.revision > max {
				max = p.revision
				head = p
			}
			return nil
		}); e != nil {
			return e
		}
		if max != count {
			return ErrEvidence
		}
		for rev := uint64(1); rev <= count; rev++ {
			if _, e := s.inspectPublicationAt(ctx, tx, endpoint, project, rev); e != nil {
				return e
			}
		}
		wantStatus := "provisional"
		if count == 0 {
			if revision != 0 || v[5] != nil || v[6] != nil {
				return ErrEvidence
			}
		} else {
			wantStatus = "active"
			if revision != max || v[5] != head.record.OperationID || v[6] != int64(head.seq) {
				return ErrEvidence
			}
		}
		n, e := inspectionCount(ctx, tx, `SELECT count(*) FROM executions x JOIN runtime_documents d ON d.operation_id=x.operation_id AND d.kind='preparation' AND d.seq=0 WHERE x.docker_endpoint_id=? AND x.project_name=? AND x.stack_id=?`, endpoint, project, stack)
		if e != nil {
			return e
		}
		if n == 0 {
			return ErrEvidence
		}
		after := ""
		for {
			rows, e := tx.QueryContext(ctx, `SELECT x.operation_id,e.latest_result_seq FROM executions x JOIN event_state e ON e.operation_id=x.operation_id WHERE x.docker_endpoint_id=? AND x.project_name=? AND x.stack_id=? AND x.operation_id>? AND e.latest_result_seq IS NOT NULL AND NOT EXISTS(SELECT 1 FROM finalizations f WHERE f.operation_id=x.operation_id) ORDER BY x.operation_id LIMIT 64`, endpoint, project, stack, after)
			if e != nil {
				return ErrStorage
			}
			refs := []struct {
				id  string
				seq uint64
			}{}
			for rows.Next() {
				var r struct {
					id  string
					seq uint64
				}
				if rows.Scan(&r.id, &r.seq) != nil {
					rows.Close()
					return ErrStorage
				}
				refs = append(refs, r)
			}
			er, cl := rows.Err(), rows.Close()
			if er != nil || cl != nil {
				return ErrStorage
			}
			if len(refs) == 0 {
				break
			}
			for _, ref := range refs {
				r, e := s.readRecord(ctx, tx, ref.id)
				if e != nil {
					return e
				}
				event, e := s.readEvent(ctx, tx, ref.id, ref.seq)
				if e != nil {
					return e
				}
				var result runtimeResult
				e = json.Unmarshal(event.Body, &result)
				clear(event.Body)
				if e != nil {
					return ErrEvidence
				}
				if result.Outcome != "not_started" {
					var c ownershipCandidate
					if e = s.readDocument(ctx, tx, r, "candidate", ref.seq, &c); e != nil {
						return e
					}
					if c.Status == "drifted" {
						wantStatus = "drifted"
					}
				}
			}
			after = refs[len(refs)-1].id
		}
		if status != wantStatus {
			return ErrEvidence
		}
		return nil
	})
}
