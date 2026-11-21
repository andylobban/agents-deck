package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/brushknight/agents-deck/backend/internal/claude"
	"github.com/brushknight/agents-deck/backend/internal/model"
	"github.com/brushknight/agents-deck/backend/internal/proc"
	"github.com/brushknight/agents-deck/backend/internal/remotestatus"
	"github.com/brushknight/agents-deck/backend/internal/store"
)

const (
	remotePollInterval = 10 * time.Second
	remotePollTimeout  = 18 * time.Second
	remoteStaleAfter   = time.Minute
	maxRemoteSubagents = 64 // bound the display list built from a remote count
)

var errRemoteReadOnly = errors.New("this session is read-only in the deck; open it in its own app")

type remoteTile struct {
	session remotestatus.Session
	seen    time.Time
	hidden  bool
}

type remoteWatch struct {
	mu    sync.Mutex
	hosts map[string]map[string]remoteTile
}

// WatchRemote starts one poller per configured host. Each host is independent,
// so an offline host cannot delay updates from the local or remote watchers.
func (d *Daemon) WatchRemote() {
	started := map[string]bool{}
	for _, config := range d.Cfg.RemoteStatusSources {
		if err := config.Validate(); err != nil {
			log.Printf("remote status source: %v", err)
			continue
		}
		if started[config.Host] {
			log.Printf("remote status source %q: duplicate host", config.Host)
			continue
		}
		started[config.Host] = true
		go d.watchRemote(config)
	}
}

func (d *Daemon) watchRemote(config remotestatus.Config) {
	var previous string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), remotePollTimeout)
		snapshot, err := remotestatus.Read(ctx, config)
		cancel()
		message := ""
		if err != nil {
			message = err.Error()
		} else {
			for _, source := range snapshot.Sources {
				if source.Status != "ok" {
					message = "one or more providers are unavailable"
					break
				}
			}
		}
		if message != previous {
			if message == "" {
				log.Printf("remote status source %q: snapshot restored", config.Host)
			} else {
				log.Printf("remote status source %q: %s", config.Host, message)
			}
		}
		previous = message
		d.syncRemote(config.Host, snapshot, time.Now())
		time.Sleep(remotePollInterval)
	}
}

func remoteID(host string, session remotestatus.Session) string {
	key := sha256.Sum256([]byte(host + "\x00" + session.Source + "\x00" + session.ID))
	return "r" + hex.EncodeToString(key[:8])
}

func (d *Daemon) syncRemote(host string, snapshot *remotestatus.Snapshot, now time.Time) {
	d.remote.mu.Lock()
	defer d.remote.mu.Unlock()
	if d.remote.hosts == nil {
		d.remote.hosts = make(map[string]map[string]remoteTile)
	}
	tiles := d.remote.hosts[host]
	if tiles == nil {
		tiles = make(map[string]remoteTile)
		d.remote.hosts[host] = tiles
	}
	seen := map[string]bool{}
	if snapshot != nil {
		for _, session := range snapshot.Sessions {
			id := remoteID(host, session)
			old, exists := tiles[id]
			tile := remoteTile{session: session, seen: now, hidden: old.hidden && old.session == session}
			tiles[id], seen[id] = tile, true
			if !tile.hidden && (!exists || old.session != session) {
				d.publishRemote(id, old.session, session)
			}
		}
	}
	for id, tile := range tiles {
		if !seen[id] && (snapshot.Authoritative(tile.session.Source, tile.session.Scope) || now.Sub(tile.seen) >= remoteStaleAfter) {
			d.Store.Remove(id)
			delete(tiles, id)
		}
	}
}

func remoteLabel(source string) string {
	switch source {
	case "hermes":
		return "Hermes"
	case "t3":
		return "T3 Code"
	default:
		return remoteText(source, 24)
	}
}

func remoteText(text string, limit int) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, text)
	return clipText(strings.Join(strings.Fields(text), " "), limit)
}

func remoteState(status string) model.Status {
	switch status {
	case "running":
		return model.Running
	case "queued":
		return model.Starting
	case "waiting":
		return model.Waiting
	case "failed":
		return model.Error
	default:
		return model.Idle
	}
}

func (d *Daemon) publishRemote(id string, old, session remotestatus.Session) {
	label := remoteLabel(session.Source)
	if label == "" {
		label = "Remote"
	}
	folder := label
	if scope := remoteText(session.Scope, 72); scope != "" {
		folder += " · " + scope
	}
	apply := func(e *store.Entry) bool {
		e.RemoteURL = remotestatus.OpenURL(session.URL)
		e.A.Title = remoteText(session.Title, 160)
		if e.A.Title == "" {
			e.A.Title = label + " session"
		}
		e.A.Folder, e.A.Model = folder, remoteText(session.Model, 80)
		e.SetStatus(remoteState(session.Status))
		e.A.Subagents = nil
		for n := range min(session.RunningSubagents, maxRemoteSubagents) {
			number := strconv.Itoa(n + 1)
			e.A.Subagents = append(e.A.Subagents, model.Subagent{ID: "s" + number, Title: "Subagent " + number})
		}
		if session.BackgroundOnly && e.A.Status == model.Running && len(e.A.Subagents) > 0 {
			claude.ShowBackground(e)
		} else if e.Background {
			e.Background, e.A.Activity = false, nil
		}
		if !session.StartedAt.IsZero() {
			e.A.StartedAt = session.StartedAt
		}
		e.MarkActiveStep(session.UpdatedAt)
		if session.Status != "finished" {
			e.A.Unseen = false
		} else if old.Status != "" && (old.Status != "finished" || session.UpdatedAt.After(old.UpdatedAt)) {
			e.A.Unseen = true
		}
		switch e.A.Status {
		case model.Waiting:
			if e.A.Waiting == nil {
				e.A.Waiting = &model.Prompt{ID: e.NextPromptID(), Kind: "input", Title: "Needs you", Context: "Open the remote session to respond."}
			}
		case model.Error:
			e.A.Error = &model.ErrInfo{Message: "The remote session failed."}
		}
		return true
	}
	if d.Store.Update(id, apply) {
		return
	}
	// The existing shell tool and external flag make this a display-only tile
	// on every client, including boards with older firmware.
	e := &store.Entry{External: true, Remote: true, A: model.Agent{ID: id, Tool: "shell", External: true}}
	apply(e)
	d.Store.Add(e)
	if !session.StartedAt.IsZero() {
		d.Store.Update(id, func(e *store.Entry) bool { e.A.StartedAt = session.StartedAt; return true })
	}
}

func (d *Daemon) focusRemote(e store.Entry) error {
	if e.RemoteURL == "" {
		return errors.New("this remote session has no browser link")
	}
	if err := proc.Open(e.RemoteURL); err != nil {
		return errors.New("could not open the remote session in your browser")
	}
	d.Store.Update(e.A.ID, func(e *store.Entry) bool {
		wasUnseen := e.A.Unseen
		e.A.Unseen = false
		return wasUnseen
	})
	return nil
}

func (d *Daemon) hideRemote(id string) {
	d.remote.mu.Lock()
	defer d.remote.mu.Unlock()
	for _, tiles := range d.remote.hosts {
		if tile, ok := tiles[id]; ok {
			tile.hidden = true
			tiles[id] = tile
			break
		}
	}
	d.Store.Remove(id)
}
