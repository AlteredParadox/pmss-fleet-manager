// pmss-fleet-manager — a single-pane dashboard for Pulsed Media seedboxes.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package collect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// genTestManager starts a manager on its own database with one enabled box
// whose SSH endpoint refuses at once (127.0.0.1:1), so every poll fails fast
// and the live state is otherwise ours to shape. It returns once the first
// poll has failed, so the run loop is asleep for the poll interval.
func genTestManager(t *testing.T) (*Manager, *store.Store, *store.Box) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	b := &store.Box{Name: "a", Host: "127.0.0.1", Port: 1, User: "u", AuthMethod: "password", Password: "x",
		Enabled: true, QbtEnabled: true, DockerEnabled: true, DuEnabled: true}
	if err := m.AddBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	waitPoll(t, m, b.ID)
	return m, st, b
}

func waitPoll(t *testing.T, m *Manager, id int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := m.State(id); ok && st.Failures > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the first poll did not fail in time")
}

// seedRemote gives the live state the facts a complete probe would have
// read from the box, as if the current generation had been probed.
func seedRemote(m *Manager, id int64) {
	m.mutateState(id, func(s *BoxState) {
		s.Ready, s.UID = true, 1000
		s.Docker.Containers = []ContainerView{{ID: "c1", Name: "web"}}
		s.Logs = LogsState{OK: true, Files: []LogFileView{{Path: ".logs/a.log"}}}
		s.Dirs = DirsState{Enabled: true, At: time.Now(), Entries: []DirEntry{{Name: "x", Bytes: 1}}}
	})
}

// An edit that keeps the endpoint keeps the facts and the readiness a
// probe established; one that moves the record to another host (or forgets
// the pin, so the next connect may reach another machine) starts from the
// record alone: no UID, no containers, no logs, no du, not ready, caches
// dropped, the old machine's alerts resolved but the renewal alert kept
// (GPT audit 2026-10-22 findings 1 and 5).
func TestMoveStartsFreshGeneration(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	seedRemote(m, b.ID)
	if err := st.SetCache(ctx, b.ID, "du", DirsState{Entries: []DirEntry{{Name: "x", Bytes: 1}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RaiseAlert(ctx, b.ID, "disk", "warn", "disk"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RaiseAlert(ctx, b.ID, "renewal", "warn", "renews"); err != nil {
		t.Fatal(err)
	}

	// Cosmetic edit: same generation.
	cos := *b
	cos.Name = "renamed"
	if err := m.UpdateBox(ctx, &cos, store.Clears{}); err != nil {
		t.Fatal(err)
	}
	s, _ := m.State(b.ID)
	if !s.Ready || s.UID != 1000 || len(s.Docker.Containers) != 1 || !s.Logs.has(".logs/a.log") || len(s.Dirs.Entries) != 1 || s.Name != "renamed" {
		t.Fatalf("cosmetic edit lost the generation's facts: ready=%v uid=%d containers=%d logs=%v dirs=%d", s.Ready, s.UID, len(s.Docker.Containers), s.Logs.Files, len(s.Dirs.Entries))
	}
	// Actions use the facts (and then fail on the refused endpoint, not on
	// readiness); a container or log the probe did not list is refused
	// before any connection.
	if err := m.ContainerAction(ctx, b.ID, "c1", "start"); err == nil || errors.Is(err, ErrNotReady) {
		t.Fatalf("container action on a ready box: %v", err)
	}
	if err := m.ContainerAction(ctx, b.ID, "c2", "start"); !errors.Is(err, errContainerUnknown) {
		t.Fatalf("unknown container: %v", err)
	}
	if _, err := m.ContainerLogs(ctx, b.ID, "c2"); !errors.Is(err, errContainerUnknown) {
		t.Fatalf("unknown container logs: %v", err)
	}
	if _, err := m.ReadLog(ctx, b.ID, ".logs/b.log"); err == nil || errors.Is(err, ErrNotReady) {
		t.Fatalf("unlisted log: %v", err)
	}
	if _, err := st.GetCache(ctx, b.ID, "du", new(DirsState)); err != nil {
		t.Fatalf("cosmetic edit dropped the du cache: %v", err)
	}

	// Move: a new generation.
	mv := cos
	mv.Host = "127.0.0.2"
	if err := m.UpdateBox(ctx, &mv, store.Clears{}); err != nil {
		t.Fatal(err)
	}
	assertFresh(t, m, st, b.ID)
	keys, _ := st.ActiveAlertKeys(ctx, b.ID)
	if !slices.Equal(keys, []string{"renewal"}) {
		t.Fatalf("active alerts after the move: %v, want only renewal", keys)
	}

	// Forgetting the pin is a move too: the same address may answer with
	// another machine.
	seedRemote(m, b.ID)
	fp := mv
	if err := m.UpdateBox(ctx, &fp, store.Clears{ForgetHostKey: true}); err != nil {
		t.Fatal(err)
	}
	assertFresh(t, m, st, b.ID)
}

func assertFresh(t *testing.T, m *Manager, st *store.Store, id int64) {
	t.Helper()
	ctx := context.Background()
	s, _ := m.State(id)
	if s.Ready || s.UID != 0 || len(s.Docker.Containers) != 0 || s.Logs.OK || len(s.Dirs.Entries) != 0 {
		t.Fatalf("moved box kept the old machine's facts: ready=%v uid=%d containers=%d logs=%v dirs=%d", s.Ready, s.UID, len(s.Docker.Containers), s.Logs.OK, len(s.Dirs.Entries))
	}
	for _, kind := range []string{"state", "du"} {
		if _, err := st.GetCache(ctx, id, kind, new(map[string]any)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s cache after the move: %v", kind, err)
		}
	}
	for name, err := range map[string]error{
		"qbt start":      must1(m.QbtAction(ctx, id, "start")),
		"qbt install":    must1(m.QbtAction(ctx, id, "install")),
		"qbt restart":    must1(m.QbtAction(ctx, id, "restart")),
		"qbt autostart":  must1(m.QbtAction(ctx, id, "autostart")),
		"qbt stop":       must1(m.QbtAction(ctx, id, "stop")),
		"container":      m.ContainerAction(ctx, id, "c1", "start"),
		"container logs": must1(m.ContainerLogs(ctx, id, "c1")),
		"read log":       must1(m.ReadLog(ctx, id, ".logs/a.log")),
		"torrent":        m.TorrentAction(ctx, id, s.BoxRev, "0123456789abcdef0123456789abcdef01234567", "stop"),
	} {
		if !errors.Is(err, ErrNotReady) {
			t.Errorf("%s on an unprobed generation: %v, want ErrNotReady", name, err)
		}
	}
}

func must1[T any](_ T, err error) error { return err }

// A cached state is restored only for the generation it was recorded under:
// a row left behind when the process died between the record update and the
// cache delete is refused by its endpoint or host-key fingerprint.
func TestRestoreRefusesOtherGeneration(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	b := &store.Box{Name: "a", Host: "New.example", Port: 22, User: "u", HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb3cxQkqM3kMvHfVWeNqK3fF3lR6x9Jh5yJQbqZLq8m"}
	if err := st.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	fp := fingerprintOrEmpty(b.HostKey)
	start := func() BoxState {
		m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer m.Stop()
		s, _ := m.State(b.ID)
		return s
	}
	for _, c := range []struct {
		name         string
		cached       BoxState
		wantRestored bool
	}{
		{"same generation", BoxState{ID: b.ID, Host: "new.example", Port: 22, User: "u", HostKeyFP: fp, UID: 1000}, true},
		{"recorded before the pin", BoxState{ID: b.ID, Host: "new.example", Port: 22, User: "u", UID: 1000}, true},
		{"other host", BoxState{ID: b.ID, Host: "old.example", Port: 22, User: "u", HostKeyFP: fp, UID: 1000}, false},
		{"other port", BoxState{ID: b.ID, Host: "new.example", Port: 2222, User: "u", HostKeyFP: fp, UID: 1000}, false},
		{"other user", BoxState{ID: b.ID, Host: "new.example", Port: 22, User: "v", HostKeyFP: fp, UID: 1000}, false},
		{"other key", BoxState{ID: b.ID, Host: "new.example", Port: 22, User: "u", HostKeyFP: "SHA256:other", UID: 1000}, false},
	} {
		if err := st.SetCache(ctx, b.ID, "state", c.cached); err != nil {
			t.Fatal(err)
		}
		s := start()
		if (s.UID == 1000) != c.wantRestored {
			t.Errorf("%s: restored=%v, want %v", c.name, s.UID == 1000, c.wantRestored)
		}
		if s.Ready {
			t.Errorf("%s: a restored state must not be ready", c.name)
		}
	}
	// A forgotten pin refuses the cache recorded under the old one.
	b.HostKey = ""
	if err := st.UpdateBox(ctx, b, store.Clears{ForgetHostKey: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCache(ctx, b.ID, "state", BoxState{ID: b.ID, Host: "new.example", Port: 22, User: "u", HostKeyFP: fp, UID: 1000}); err != nil {
		t.Fatal(err)
	}
	if s := start(); s.UID == 1000 {
		t.Error("the cache recorded under a forgotten pin was restored")
	}
}

func TestSameGeneration(t *testing.T) {
	b := &store.Box{Host: "H.example", Port: 22, User: "u", HostKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb3cxQkqM3kMvHfVWeNqK3fF3lR6x9Jh5yJQbqZLq8m"}
	fp := fingerprintOrEmpty(b.HostKey)
	for gen, want := range map[string]bool{
		"h.example:22/u@" + fp:    true,
		"h.example:22/u@":         true, // before the pin
		"h.example:22/u@SHA256:x": false,
		"h.example:23/u@" + fp:    false,
		"other:22/u@" + fp:        false,
		"":                        false,
	} {
		if got := sameGeneration(gen, b); got != want {
			t.Errorf("sameGeneration(%q) = %v, want %v", gen, got, want)
		}
	}
	b.HostKey = ""
	if sameGeneration("h.example:22/u@"+fp, b) {
		t.Error("a measurement under a pin the record no longer holds was accepted")
	}
}

// A du that did not complete leaves the last good cache row alone (its row
// time is the measurement time) and only reports the error on the live
// state; a completed one is tagged with the generation (finding 6).
func TestFinishDuKeepsLastGoodOnInterruption(t *testing.T) {
	ctx := context.Background()
	m, boxes := revTestManager(t, 1)
	b := boxes[0]
	p := m.poller(b.ID)
	old := DirsState{Entries: []DirEntry{{Name: "old", Bytes: 7}}, Gen: p.generation()}
	if err := m.st.SetCache(ctx, b.ID, "du", old); err != nil {
		t.Fatal(err)
	}
	at, _ := m.st.GetCache(ctx, b.ID, "du", new(DirsState))
	time.Sleep(1100 * time.Millisecond) // the row time has second resolution
	if p.finishDu(ctx, []byte("5\tpartial\n"), errors.New("ssh: session closed")) {
		t.Fatal("an interrupted du counted as completed")
	}
	var cached DirsState
	at2, err := m.st.GetCache(ctx, b.ID, "du", &cached)
	if err != nil || !at2.Equal(at) || len(cached.Entries) != 1 || cached.Entries[0].Name != "old" {
		t.Fatalf("the interrupted du touched the cache: %v %v %+v", at2, err, cached)
	}
	s, _ := m.State(b.ID)
	if s.Dirs.Error == "" || s.Dirs.Running {
		t.Fatalf("live state after the interruption: %+v", s.Dirs)
	}
	if !p.finishDu(ctx, []byte("9\tfresh\n"), nil) {
		t.Fatal("a complete du did not count")
	}
	if _, err := m.st.GetCache(ctx, b.ID, "du", &cached); err != nil || cached.Gen != p.generation() || len(cached.Entries) != 1 || cached.Entries[0].Name != "fresh" {
		t.Fatalf("completed du: %v %+v", err, cached)
	}
	s, _ = m.State(b.ID)
	if len(s.Dirs.Entries) != 1 || s.Dirs.Entries[0].Name != "fresh" || s.Dirs.Error != "" {
		t.Fatalf("live state after the measurement: %+v", s.Dirs)
	}
	// A failure schedules a retry sooner than the interval, not a full one.
	p.duDone(false)
	if !p.lastDu.IsZero() || time.Until(p.duRetryAt) > duRetry {
		t.Fatalf("after a failure lastDu=%v retryAt=%v", p.lastDu, p.duRetryAt)
	}
	p.duDone(true)
	if p.lastDu.IsZero() {
		t.Fatal("a completed measurement did not set lastDu")
	}
}

// A retired poller's du worker is joined before stopAndWait returns, so the
// poller an edit starts in its place never races it.
func TestStopAndWaitJoinsDuWorker(t *testing.T) {
	m, boxes := revTestManager(t, 1)
	p := m.poller(boxes[0].ID)
	p.duWG.Add(1)
	released := make(chan struct{})
	go func() {
		<-released
		p.duWG.Done()
	}()
	done := make(chan struct{})
	go func() {
		p.stopAndWait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("stopAndWait returned while the du worker was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(released)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopAndWait did not return after the worker finished")
	}
}

// The rootless Docker client is dialled for a UID; a probe that reports
// another UID (the record was moved to another account) gets a new one.
func TestDockerClientKeyedByUID(t *testing.T) {
	p := newPoller(&Manager{}, &store.Box{ID: 1})
	a := p.dockerClient(1000)
	if p.dockerClient(1000) != a {
		t.Fatal("same UID, new client")
	}
	b := p.dockerClient(2000)
	if b == a || p.dockerClient(2000) != b {
		t.Fatal("the client for another UID was not rebuilt")
	}
}

// maybeDu loads a cached measurement only for the box's own generation; a
// row with no tag (written before the tag existed) cannot be placed and is
// dropped rather than trusted (Codex on PR 101).
func TestMaybeDuDropsUntaggedCache(t *testing.T) {
	ctx := context.Background()
	m, boxes := revTestManager(t, 1)
	p := m.poller(boxes[0].ID)
	for _, c := range []struct {
		gen  string
		want int
	}{{"", 0}, {"other:22/u@", 0}, {p.generation(), 1}} {
		if err := m.st.SetCache(ctx, p.box.ID, "du", DirsState{Gen: c.gen, Entries: []DirEntry{{Name: "x", Bytes: 1}}}); err != nil {
			t.Fatal(err)
		}
		st := BoxState{}
		p.maybeDu(ctx, &st)
		if len(st.Dirs.Entries) != c.want {
			t.Errorf("gen %q: %d entries loaded, want %d", c.gen, len(st.Dirs.Entries), c.want)
		}
	}
}

// A du whose stdout overran the cap is an interrupted run even when du
// itself exited 1 for an unreadable entry: the last good cache row stays,
// the prefix is never parsed as a measurement (fred on PR 103).
func TestFinishDuRejectsOverflowedOutput(t *testing.T) {
	ctx := context.Background()
	m, boxes := revTestManager(t, 1)
	p := m.poller(boxes[0].ID)
	old := DirsState{Entries: []DirEntry{{Name: "old", Bytes: 7}}, Gen: p.generation()}
	if err := m.st.SetCache(ctx, boxes[0].ID, "du", old); err != nil {
		t.Fatal(err)
	}
	overflow := fmt.Errorf("%w (16 MiB kept; the command then reported: Process exited with status 1)", sshx.ErrOutputLimit)
	if p.finishDu(ctx, []byte("5\tprefix\n"), overflow) {
		t.Fatal("an overflowed du counted as completed")
	}
	var cached DirsState
	if _, err := m.st.GetCache(ctx, boxes[0].ID, "du", &cached); err != nil || len(cached.Entries) != 1 || cached.Entries[0].Name != "old" {
		t.Fatalf("the overflowed du touched the cache: %v %+v", err, cached)
	}
	if s, _ := m.State(boxes[0].ID); !strings.Contains(s.Dirs.Error, "exceeded the limit") {
		t.Fatalf("live state after the overflow: %+v", s.Dirs)
	}
}

// A torrent button carries the box record revision it was drawn at; after
// an edit (here a move to another host that has since been probed, so the
// box is ready again) the old revision is refused before anything reaches
// the box, while the current one gets past the check (daybreak blue audit
// of 7506bd4).
func TestTorrentActionNeedsCurrentBoxRevision(t *testing.T) {
	ctx := context.Background()
	m, _, b := genTestManager(t)
	seedRemote(m, b.ID)
	before, _ := m.State(b.ID)
	if before.BoxRev != b.Revision {
		t.Fatalf("state box_rev %d, record %d", before.BoxRev, b.Revision)
	}
	// A cosmetic edit keeps the measured state and must still publish the
	// record's new revision, or every later action would be refused (Codex
	// on PR 130).
	cos := *b
	cos.Name = "renamed"
	if err := m.UpdateBox(ctx, &cos, store.Clears{}); err != nil {
		t.Fatal(err)
	}
	renamed, _ := m.State(b.ID)
	if !renamed.Ready || renamed.BoxRev != cos.Revision || renamed.BoxRev == before.BoxRev {
		t.Fatalf("cosmetic edit: ready=%v box_rev %d, record %d", renamed.Ready, renamed.BoxRev, cos.Revision)
	}
	if err := m.TorrentAction(ctx, b.ID, renamed.BoxRev, strings.Repeat("a", 40), "stop"); errors.Is(err, ErrStaleBox) || errors.Is(err, ErrNotReady) {
		t.Fatalf("current revision after a rename: %v", err)
	}
	mv := cos
	mv.Host = "127.0.0.2"
	if err := m.UpdateBox(ctx, &mv, store.Clears{}); err != nil {
		t.Fatal(err)
	}
	seedRemote(m, b.ID)
	after, _ := m.State(b.ID)
	if after.BoxRev != mv.Revision {
		t.Fatalf("move: box_rev %d, record %d", after.BoxRev, mv.Revision)
	}
	if after.BoxRev == before.BoxRev {
		t.Fatalf("an edit must change box_rev (%d)", after.BoxRev)
	}
	hash := strings.Repeat("a", 40)
	for _, action := range []string{"stop", "remove-data"} {
		if err := m.TorrentAction(ctx, b.ID, before.BoxRev, hash, action); !errors.Is(err, ErrStaleBox) {
			t.Fatalf("qbt %s with the old revision: %v", action, err)
		}
	}
	if err := m.TorrentAction(ctx, b.ID, after.BoxRev, hash, "stop"); err == nil || errors.Is(err, ErrStaleBox) {
		t.Fatalf("current revision: %v, want past the check (then the refused endpoint)", err)
	}
}

// Editing a ready box to an rTorrent socket outside the home keeps the
// box ready (same machine) but leaves no endpoint for actions until a poll
// resolves and checks it, so neither an add nor a torrent action can dial
// the edited socket in between (fred / Codex on PR 130).
func TestEditedSocketUnusableUntilPolled(t *testing.T) {
	ctx := context.Background()
	m, _, b := genTestManager(t)
	seedRemote(m, b.ID)
	m.mutateState(b.ID, func(s *BoxState) {
		s.Rt = RtState{Enabled: true, Endpoint: "unix:/home/u/.rtorrent.socket", Reachable: true}
	})
	ed := *b
	ed.RtEnabled, ed.RtSCGI = true, "unix:/tmp/other.sock"
	if err := m.UpdateBox(ctx, &ed, store.Clears{}); err != nil {
		t.Fatal(err)
	}
	s, _ := m.State(b.ID)
	if !s.Ready || s.Rt.Endpoint != "" {
		t.Fatalf("ready=%v endpoint %q", s.Ready, s.Rt.Endpoint)
	}
	if _, err := m.RtAddTorrents(ctx, b.ID, qbt.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}}); err == nil || !strings.Contains(err.Error(), "no SCGI endpoint") {
		t.Fatalf("add: %v", err)
	}
	if err := m.RtTorrentAction(ctx, b.ID, s.BoxRev, strings.Repeat("a", 40), "remove-data"); err == nil || !strings.Contains(err.Error(), "no SCGI endpoint") {
		t.Fatalf("remove-data: %v", err)
	}
}

// An edit that changes a client's target drops that client's carried rows,
// so no row drawn from the old target ever carries the new revision; the
// next poll lists the new target's. Other edits keep them (fred review on
// PR 130).
func TestRetargetDropsCarriedRows(t *testing.T) {
	ctx := context.Background()
	m, _, b := genTestManager(t)
	h := strings.Repeat("a", 40)
	seed := func() {
		seedRemote(m, b.ID)
		m.mutateState(b.ID, func(s *BoxState) {
			s.Qbt.Torrents = []TorrentView{{Hash: h, Name: "Q"}}
			s.Rt.Torrents = []TorrentView{{Hash: h, Name: "R"}}
		})
	}
	edit := func(f func(*store.Box)) BoxState {
		cur, _ := m.st.GetBox(ctx, b.ID)
		f(cur)
		if err := m.UpdateBox(ctx, cur, store.Clears{}); err != nil {
			t.Fatal(err)
		}
		s, _ := m.State(b.ID)
		return s
	}
	seed()
	if s := edit(func(x *store.Box) { x.Name = "renamed" }); len(s.Qbt.Torrents) != 1 || len(s.Rt.Torrents) != 1 {
		t.Fatalf("rename dropped rows: %d %d", len(s.Qbt.Torrents), len(s.Rt.Torrents))
	}
	seed()
	if s := edit(func(x *store.Box) { x.RtEnabled, x.RtSCGI = true, "unix:/home/u/other.sock" }); !s.Ready || len(s.Rt.Torrents) != 0 || len(s.Qbt.Torrents) != 1 {
		t.Fatalf("socket edit: ready=%v rt %d qbt %d", s.Ready, len(s.Rt.Torrents), len(s.Qbt.Torrents))
	}
	seed()
	if s := edit(func(x *store.Box) { x.QbtPort = 8081 }); len(s.Qbt.Torrents) != 0 || len(s.Rt.Torrents) != 1 {
		t.Fatalf("port edit: qbt %d rt %d", len(s.Qbt.Torrents), len(s.Rt.Torrents))
	}
}
