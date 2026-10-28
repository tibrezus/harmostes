package sessionstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stubActors records the last publish payload and serves it back on
// fetch — the PRLineage actor's contract in miniature (turn-serialized,
// server-authoritative generation).
type stubActors struct {
	stored map[string][]byte
	gen    map[string]int
}

func newStubActors() *stubActors {
	return &stubActors{stored: map[string][]byte{}, gen: map[string]int{}}
}

func (s *stubActors) InvokeActor(_ context.Context, actorType, actorID, method string, payload []byte) ([]byte, error) {
	key := actorType + "/" + actorID
	switch method {
	case "fetch":
		return s.stored[key], nil
	case "publish":
		// mirror the actor: generation is server-authoritative, set
		// INSIDE the turn before the state is stored
		s.gen[key]++
		var v map[string]any
		_ = json.Unmarshal(payload, &v)
		v["generation"] = s.gen[key]
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		s.stored[key] = b
		return []byte(`{"generation":` + itoa(s.gen[key]) + `}`), nil
	}
	return nil, nil
}

// TestCarryRoundTripNoTranscript pins the ADR-0022 invariant at the store
// boundary: the published lineage carries the TRACE (bounded carry), and
// the transcript is GONE from the durable payload — the next round cannot
// resume what was never stored.
func TestCarryRoundTripNoTranscript(t *testing.T) {
	actors := newStubActors()
	store := Store{Actors: actors}

	// A round that produced a large transcript publishes its carry.
	transcript := strings.Repeat("{\"type\":\"message\"}\n", 100000)
	carry := ExtractCarry([]byte(transcript), "", "deadbeef1234")
	if err := store.Publish(context.Background(), "git.rezus.cloud/tibrez/rhesadox", "99", Lineage{Carry: carry, LastHead: "deadbeef1234"}); err != nil {
		t.Fatal(err)
	}

	// The stored blob must not contain transcript-scale content.
	id, err := ActorID("git.rezus.cloud/tibrez/rhesadox", "99")
	if err != nil {
		t.Fatal(err)
	}
	blob := string(actors.stored["PRLineage/"+id])
	if len(blob) > MaxCarryBytes+256 { // + JSON envelope slack
		t.Errorf("durable payload is %d bytes — the carry bound leaked", len(blob))
	}

	// The next round fetches the carry and a ZERO transcript.
	l, err := store.Fetch(context.Background(), "git.rezus.cloud/tibrez/rhesadox", "99")
	if err != nil {
		t.Fatal(err)
	}
	if l.Session != "" {
		t.Errorf("fetched lineage carries a transcript (%d bytes) — resume is not dead", len(l.Session))
	}
	if !strings.Contains(l.Carry, "deadbeef1234") {
		t.Errorf("fetched carry lost the round trace:\n%s", l.Carry[:minInt(200, len(l.Carry))])
	}
	if l.Generation != 1 {
		t.Errorf("generation must still advance server-side, got %d", l.Generation)
	}
}

// TestLegacyLineageServesRawForTheFold pins the one-way migration: a
// lineage stored by the OLD code (raw session) is still a readable VIEW
// at fetch — the caller folds it to a carry once, and nothing ever
// materializes it as a session file again (Materialize is gone).
func TestLegacyLineageServesRawForTheFold(t *testing.T) {
	actors := newStubActors()
	id, _ := ActorID("git.rezus.cloud/tibrez/rhesadox", "98")
	actors.stored["PRLineage/"+id] = []byte(`{"session":"{\"type\":\"session\",\"id\":\"harmostes-98\"}","lastHead":"legacy","generation":1,"file":"2026-01-01T00-00-00-000Z_harmostes-98.jsonl"}`)

	l, err := (Store{Actors: actors}).Fetch(context.Background(), "git.rezus.cloud/tibrez/rhesadox", "98")
	if err != nil {
		t.Fatal(err)
	}
	if l.Session == "" {
		t.Fatal("the legacy lineage must serve its raw session to the fold path")
	}
	if l.Carry != "" {
		t.Error("a legacy lineage carries no carry yet")
	}
	carry := ExtractCarry([]byte(l.Session), "", l.LastHead)
	if !strings.Contains(carry, "head: legacy") {
		t.Errorf("legacy fold lost the trace:\n%s", carry)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
