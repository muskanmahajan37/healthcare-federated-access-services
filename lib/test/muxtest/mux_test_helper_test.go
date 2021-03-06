// Copyright 2020 Google LLC.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package muxtest

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp" /* copybara-comment */
	"github.com/gorilla/mux" /* copybara-comment */
	"bitbucket.org/creachadair/stringset" /* copybara-comment */
)

func TestPathsInRouter(t *testing.T) {
	r := mux.NewRouter()
	r.HandleFunc("/all", f)
	r.HandleFunc("/get", f).Methods(http.MethodGet)
	r.HandleFunc("/getpost", f).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc("/putdel", f).Methods(http.MethodPut)
	r.HandleFunc("/putdel", f).Methods(http.MethodDelete)

	got := PathsInRouter(t, r)
	want := stringset.New(
		"/all",
		"GET /get",
		"GET|POST /getpost",
		"PUT /putdel",
		"DELETE /putdel",
	)
	if d := cmp.Diff(want, got); len(d) > 0 {
		t.Errorf("PathsInRouter() (-want, +got): %s", d)
	}
}

func f(w http.ResponseWriter, r *http.Request) {}
