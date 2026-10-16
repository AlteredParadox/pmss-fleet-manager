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

package api

import (
	"net/http"
	"regexp"
)

// sourceBaseURL is where the Corresponding Source is published. AGPL §13
// requires a network user be offered the source of the version they are
// interacting with — a downstream fork that changes this (and rebuilds) makes
// /source point at ITS source, which is exactly the intent.
const sourceBaseURL = "https://github.com/AlteredParadox/pmss-fleet-manager"

// SetLegal injects the embedded license texts (see legal.go at the repo root).
func (s *Server) SetLegal(license, thirdParty []byte) {
	s.license = license
	s.thirdParty = thirdParty
}

// SetVersion records the build's version so /source can pin its offer to the
// exact revision this binary was built from.
func (s *Server) SetVersion(v string) { s.version = v }

// Version returns the build version ("dev" for unstamped builds).
func (s *Server) Version() string {
	if s.version == "" {
		return "dev"
	}
	return s.version
}

// pinnable matches a release tag ("v1.2.3") or a bare commit hash, the two
// version strings that name a tree on GitHub. `git describe` output such as
// "v1.2.3-4-gabc1234" or "abc1234-dirty" does not, and must not be pinned.
var pinnable = regexp.MustCompile(`^(v\d+\.\d+\.\d+|[0-9a-f]{7,40})$`)

// sourceURL is the AGPL §13 offer target. A stamped release pins to its tag,
// so the source you are offered is the source that is running; any other
// build can only point at the repository.
func (s *Server) sourceURL() string {
	if v := s.Version(); pinnable.MatchString(v) {
		return sourceBaseURL + "/tree/" + v
	}
	return sourceBaseURL
}

// isPublicPath reports whether a route is served without basic auth. AGPL
// §13 obliges the source offer to every network user, and a login wall
// would defeat it.
func isPublicPath(p string) bool {
	return p == "/license" || p == "/third-party-licenses" || p == "/source"
}

// serveLegalText serves an embedded text blob as plain UTF-8. The body is
// read through a closure at REQUEST time, so route registration does not
// have to happen after SetLegal.
func (s *Server) serveLegalText(get func() []byte, missing string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := get()
		if len(body) == 0 {
			http.Error(w, missing, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Tied to the binary, so it only changes when the build does.
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(body)
	}
}

// handleSource implements the AGPL §13 source offer.
func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.sourceURL(), http.StatusFound)
}
