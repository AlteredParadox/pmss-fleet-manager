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

package sshx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func writeKey(t *testing.T, path string) ssh.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return sp
}

// The -ssh-key default sits between the box's own key path (which always
// wins, alone) and the usual ~/.ssh names (still tried after it); a
// default that is missing or not a key is named in the error, not fatal.
func TestDefaultKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() { DefaultKey = "" })
	std := writeKey(t, filepath.Join(home, ".ssh", "id_ed25519"))
	glob := writeKey(t, filepath.Join(home, "secrets", "fleet"))
	own := writeKey(t, filepath.Join(home, "own"))
	fp := func(signers []ssh.Signer) []string {
		var out []string
		for _, s := range signers {
			out = append(out, ssh.FingerprintSHA256(s.PublicKey()))
		}
		return out
	}
	// Unset: the usual names only, as before.
	s, _, err := keySigners(Config{})
	if err != nil || strings.Join(fp(s), ",") != ssh.FingerprintSHA256(std) {
		t.Fatalf("no default: %v %v", fp(s), err)
	}
	// Set: tried first, then the usual names.
	DefaultKey = filepath.Join(home, "secrets", "fleet")
	s, _, err = keySigners(Config{})
	if err != nil || strings.Join(fp(s), ",") != ssh.FingerprintSHA256(glob)+","+ssh.FingerprintSHA256(std) {
		t.Fatalf("default first: %v %v", fp(s), err)
	}
	// The box's own path wins, alone.
	s, _, err = keySigners(Config{KeyPath: filepath.Join(home, "own")})
	if err != nil || strings.Join(fp(s), ",") != ssh.FingerprintSHA256(own) {
		t.Fatalf("box path: %v %v", fp(s), err)
	}
	// A missing default, or one that is not a key, is reported, not fatal.
	DefaultKey = filepath.Join(home, "secrets", "missing")
	s, notes, err := keySigners(Config{})
	if err != nil || len(s) != 1 || len(notes) != 1 || !strings.Contains(notes[0], "the -ssh-key default: no such file") {
		t.Fatalf("missing default: %v %v %v", fp(s), notes, err)
	}
	os.WriteFile(filepath.Join(home, "secrets", "junk"), []byte("not a key"), 0o600)
	DefaultKey = filepath.Join(home, "secrets", "junk")
	_, notes, _ = keySigners(Config{})
	if len(notes) != 1 || !strings.Contains(notes[0], "not an unencrypted private key") {
		t.Fatalf("junk default: %v", notes)
	}
	// With key auth selected and nothing usable, the error names it.
	os.Remove(filepath.Join(home, ".ssh", "id_ed25519"))
	if _, _, err := keyMethods(Config{AuthMethod: "key"}); err == nil || !strings.Contains(err.Error(), "could not use "+DefaultKey+" (the -ssh-key default") {
		t.Fatalf("key auth: %v", err)
	}
	// "~/" is the home, as for a box's key path.
	DefaultKey = "~/secrets/fleet"
	if p := keyPaths(Config{}); p[0] != filepath.Join(home, "secrets", "fleet") {
		t.Fatalf("tilde: %v", p)
	}
}

// fred on PR 128: a real handshake where the key files offer a valid key
// the server refuses (the -ssh-key default, authorized on another box)
// and only the agent holds the accepted one. The agent must still be
// tried: x/crypto skips a second method of the same name once one has
// been refused, so files and agent share one public-key method. An
// explicit auth mode still keeps to its own source.
func TestAgentAfterRefusedDefaultKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no ~/.ssh keys at all
	t.Cleanup(func() { DefaultKey = "" })
	DefaultKey = filepath.Join(home, "fleet")
	writeKey(t, DefaultKey) // valid, not authorized here

	// The agent holds the only key the server accepts.
	_, agentPriv, _ := ed25519.GenerateKey(rand.Reader)
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: agentPriv}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "agent.sock")
	aln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aln.Close() })
	go func() {
		for {
			c, err := aln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); agent.ServeAgent(ring, c) }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
	agentPub, _ := ssh.NewPublicKey(agentPriv.Public())

	// A server that accepts only the agent's key.
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	var mu sync.Mutex
	var offered []string
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		mu.Lock()
		offered = append(offered, ssh.FingerprintSHA256(k))
		mu.Unlock()
		if bytes.Equal(k.Marshal(), agentPub.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("not authorized")
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					conn.Close()
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					nc.Reject(ssh.Prohibited, "test")
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	dial := func(mode string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := Dial(ctx, Config{Host: "127.0.0.1", Port: port, User: "u", AuthMethod: mode})
		if err == nil {
			c.Close()
		}
		return err
	}
	if err := dial(""); err != nil {
		t.Fatalf("auto: the agent was not tried after the default key was refused: %v (offered %v)", err, offered)
	}
	mu.Lock()
	if len(offered) < 2 || offered[len(offered)-1] != ssh.FingerprintSHA256(agentPub) {
		t.Fatalf("expected the default key first, then the agent's: %v", offered)
	}
	mu.Unlock()
	if err := dial("key"); err == nil {
		t.Fatal("key mode used the agent")
	}
	if err := dial("agent"); err != nil {
		t.Fatalf("agent mode: %v", err)
	}
}

// Codex on PR 128: in auto mode an agent that accepts the connection but
// never answers must not block a key file that works; the wait is bounded
// and the key file still authenticates.
func TestSilentAgentDoesNotBlockKeyFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	pub := writeKey(t, filepath.Join(home, ".ssh", "id_ed25519"))
	old := agentWait
	agentWait = 300 * time.Millisecond
	t.Cleanup(func() { agentWait = old })
	sock := filepath.Join(t.TempDir(), "agent.sock")
	aln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aln.Close() })
	go func() {
		for {
			c, err := aln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }() // reads, never answers
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", sock)
	port := keyServer(t, pub)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	c, err := Dial(ctx, Config{Host: "127.0.0.1", Port: port, User: "u"})
	if err != nil {
		t.Fatalf("auto mode with a silent agent: %v", err)
	}
	c.Close()
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took %s", el)
	}
	// Explicit agent mode still waits on its only source, until the context.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel2()
	if _, err := Dial(ctx2, Config{Host: "127.0.0.1", Port: port, User: "u", AuthMethod: "agent"}); err == nil {
		t.Fatal("agent mode succeeded without an agent answer")
	}
}

// keyServer runs an SSH server that accepts only pub and returns its port.
func keyServer(t *testing.T, pub ssh.PublicKey) int {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(k.Marshal(), pub.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("not authorized")
	}}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					conn.Close()
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					nc.Reject(ssh.Prohibited, "test")
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}
