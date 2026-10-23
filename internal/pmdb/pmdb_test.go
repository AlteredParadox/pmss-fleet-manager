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

package pmdb

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Le4-0-105-238louhi.PulsedMedia.com.": "le4-0-105-238louhi.pulsedmedia.com",
		" odin.pulsedmedia.com ":              "odin.pulsedmedia.com",
		"odin":                                "odin",
		"":                                    "",
		"-bad.example":                        "",
		"bad-.example":                        "",
		"under_score.example":                 "",
		"a..b":                                "",
		"sp ace.example":                      "",
	} {
		got, ok := Normalize(in)
		if got != want || ok != (want != "") {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestListAndSubmit(t *testing.T) {
	var gotAuth, gotCT string
	var gotRow map[string]any
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/servers":
			if r.Method != http.MethodGet {
				t.Errorf("list method %s", r.Method)
			}
			_, _ = io.WriteString(w, `[{"serverName":"Odin.pulsedmedia.com","coreCount":16},{"serverName":"thor.pulsedmedia.com"}]`)
		case "/api/servers/raw":
			gotAuth, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &gotRow); err != nil {
				t.Errorf("body: %v", err)
			}
			w.WriteHeader(status)
			if status >= 400 {
				_, _ = io.WriteString(w, `{"error":"This server already exists."}`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL + "/"}
	ctx := context.Background()

	listed, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !listed["odin.pulsedmedia.com"] || !listed["thor.pulsedmedia.com"] || len(listed) != 2 {
		t.Fatalf("listed: %v", listed)
	}

	mhz := 2100.0
	row := Row{ServerName: "loki.pulsedmedia.com", ProcessorName: "Intel(R) Core(TM) i7-12700", CoreCount: 20,
		ProcessorMHz: &mhz, RAMGiB: 62.5, StorageTiB: 13.64, QSV: true}
	if err := c.Submit(ctx, " key-words-that-pass ", row); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer key-words-that-pass" || gotCT != "application/json" {
		t.Fatalf("headers: %q %q", gotAuth, gotCT)
	}
	// Every field the catalogue requires goes as a JSON number or bool;
	// unknown optionals go as null, never as 0, and nothing extra goes.
	want := map[string]any{"serverName": "loki.pulsedmedia.com", "processorName": "Intel(R) Core(TM) i7-12700", "coreCount": 20.0,
		"processorMhz": 2100.0, "ramGiB": 62.5, "swapGiB": nil, "storageTiB": 13.64, "qsv": true}
	if len(gotRow) != len(want) {
		t.Fatalf("row keys: %v", gotRow)
	}
	for k, v := range want {
		if gotRow[k] != v {
			t.Errorf("row[%s] = %v, want %v", k, gotRow[k], v)
		}
	}
	if _, has := gotRow["replace"]; has {
		t.Error("replace must never be sent")
	}

	status = http.StatusConflict
	if err := c.Submit(ctx, "k", row); !errors.Is(err, ErrExists) {
		t.Fatalf("409: %v", err)
	}
	status = http.StatusUnauthorized
	if err := c.Submit(ctx, "k", row); !errors.Is(err, ErrAuth) {
		t.Fatalf("401: %v", err)
	}
	status = http.StatusTooManyRequests
	if err := c.Submit(ctx, "k", row); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	status = http.StatusBadRequest
	if err := c.Submit(ctx, "k", row); err == nil || err.Error() != "HTTP 400: This server already exists." {
		t.Fatalf("400: %v", err)
	}
	if err := c.Submit(ctx, "", row); err == nil {
		t.Fatal("empty key must not be sent")
	}
}

func TestRedirectRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/api/servers", http.StatusFound)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("redirect followed")
	}
	if err := c.Submit(context.Background(), "k", Row{ServerName: "x"}); err == nil {
		t.Fatal("redirect followed with the key")
	}
}
