package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/brushknight/agents-deck/backend/internal/model"
	"github.com/brushknight/agents-deck/backend/internal/remotestatus"
	"github.com/brushknight/agents-deck/backend/internal/store"
)

func TestRemoteTilesReconcileWithoutMutatingSessions(t *testing.T) {
	d := &Daemon{Store: store.New("", "test", "dev")}
	now := time.Now().UTC()
	first := remotestatus.Session{
		ID: "123", Source: "hermes", Scope: "alpha", Title: "First\nchat", Status: "waiting",
		URL: "https://example.test/chat?resume=123",
	}
	second := remotestatus.Session{ID: "123", Source: "t3", Scope: "project", Status: "running"}
	snap := &remotestatus.Snapshot{
		Sources:  map[string]remotestatus.Source{"hermes": {Status: "ok"}, "t3": {Status: "ok"}},
		Sessions: []remotestatus.Session{first, second},
	}
	d.syncRemote("desk", snap, now)
	d.syncRemote("lab", snap, now)
	firstID, secondID := remoteID("desk", first), remoteID("desk", second)
	entry, ok := d.Store.Get(firstID)
	if !ok || !entry.External || entry.A.App != "" || entry.A.Title != "First chat" || entry.A.Status != model.Waiting || entry.A.AttentionSlot == 0 {
		t.Fatalf("remote waiting tile: %+v", entry.A)
	}
	if prompt := entry.A.Waiting; prompt == nil || len(prompt.Options) != 0 {
		t.Fatalf("remote question should have no answer controls: %+v", prompt)
	}
	if err := d.Answer(firstID, "p1", "1"); !errors.Is(err, errRemoteReadOnly) {
		t.Fatalf("answer: %v", err)
	}
	if err := d.Interrupt(secondID); !errors.Is(err, errRemoteReadOnly) {
		t.Fatalf("interrupt: %v", err)
	}
	if err := d.Dismiss(firstID); err != nil {
		t.Fatal(err)
	}
	d.syncRemote("desk", snap, now.Add(time.Second))
	if _, ok := d.Store.Get(firstID); ok {
		t.Fatal("unchanged session should remain hidden")
	}
	snap.Sessions[0].Status = "running"
	snap.Sessions[1].Status = "finished"
	d.syncRemote("desk", snap, now.Add(2*time.Second))
	if item, ok := d.Store.Get(firstID); !ok || item.A.Status != model.Running || item.A.Waiting != nil {
		t.Fatal("changed remote session did not return")
	}
	if item, ok := d.Store.Get(secondID); !ok || item.A.Status != model.Idle || !item.A.Unseen {
		t.Fatal("observed finish should be marked unseen")
	}
	if _, ok := d.Store.Get(remoteID("lab", first)); !ok {
		t.Fatal("same remote identity on another SSH host was lost")
	}
}

func TestRemotePartialFailureExpiresOnlyUnconfirmedTiles(t *testing.T) {
	d := &Daemon{Store: store.New("", "test", "dev")}
	now := time.Now().UTC()
	alpha := remotestatus.Session{ID: "a", Source: "hermes", Scope: "alpha", Status: "waiting"}
	beta := remotestatus.Session{ID: "b", Source: "hermes", Scope: "beta", Status: "running"}
	t3 := remotestatus.Session{ID: "c", Source: "t3", Scope: "project", Status: "running"}
	d.syncRemote("host", &remotestatus.Snapshot{
		Sources:  map[string]remotestatus.Source{"hermes": {Status: "ok"}, "t3": {Status: "ok"}},
		Sessions: []remotestatus.Session{alpha, beta, t3},
	}, now)
	partial := &remotestatus.Snapshot{
		Sources: map[string]remotestatus.Source{
			"hermes": {Status: "partial", Scopes: []string{"alpha"}}, "t3": {Status: "ok"},
		},
		Sessions: []remotestatus.Session{t3},
	}
	d.syncRemote("host", partial, now.Add(time.Second))
	if _, ok := d.Store.Get(remoteID("host", alpha)); ok {
		t.Fatal("missing session from a healthy scope should disappear")
	}
	if _, ok := d.Store.Get(remoteID("host", beta)); !ok {
		t.Fatal("short source failure should keep the last known tile")
	}
	d.syncRemote("host", partial, now.Add(remoteStaleAfter))
	if items := d.Store.Entries(); len(items) != 1 || items[0].A.ID != remoteID("host", t3) {
		t.Fatalf("expected the healthy source only after timeout, got %d tiles", len(items))
	}
	d.syncRemote("host", nil, now.Add(2*remoteStaleAfter))
	if len(d.Store.Entries()) != 0 {
		t.Fatal("transport failure should eventually clear the stale host")
	}
}

func TestRemoteSubagentActivityDistinguishesTheParentTurn(t *testing.T) {
	d := &Daemon{Store: store.New("", "test", "dev")}
	now := time.Now().UTC()
	parent := remotestatus.Session{ID: "parent", Source: "hermes", Scope: "alpha", Status: "running", RunningSubagents: 2, BackgroundOnly: true}
	snap := &remotestatus.Snapshot{
		Sources:  map[string]remotestatus.Source{"hermes": {Status: "ok"}},
		Sessions: []remotestatus.Session{parent},
	}
	id := remoteID("host", parent)
	d.syncRemote("host", snap, now)
	if item, ok := d.Store.Get(id); !ok || !item.Background || item.A.Activity == nil ||
		item.A.Activity.Detail != "2 subagents working" || len(item.A.Subagents) != 2 {
		t.Fatalf("child-only activity should show a count and indicator: %+v", item.A)
	}

	snap.Sessions[0].BackgroundOnly = false
	d.syncRemote("host", snap, now.Add(time.Second))
	if item, ok := d.Store.Get(id); !ok || item.Background || item.A.Activity != nil || len(item.A.Subagents) != 2 {
		t.Fatalf("parent working with children should keep the indicator without child-only activity: %+v", item.A)
	}

	snap.Sessions[0].Status = "waiting"
	d.syncRemote("host", snap, now.Add(2*time.Second))
	if item, ok := d.Store.Get(id); !ok || item.A.Waiting == nil || len(item.A.Subagents) != 2 {
		t.Fatalf("waiting parent should keep its prompt and child indicator: %+v", item.A)
	}

	snap.Sessions[0].Status, snap.Sessions[0].RunningSubagents = "idle", 0
	d.syncRemote("host", snap, now.Add(3*time.Second))
	if item, ok := d.Store.Get(id); !ok || item.A.Status != model.Idle || len(item.A.Subagents) != 0 {
		t.Fatalf("child indicator should clear when the count drops to zero: %+v", item.A)
	}
}
