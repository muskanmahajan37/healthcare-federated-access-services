// Copyright 2019 Google LLC
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

package httputils

import (
	"net/http"
	"strings"

	"google3/net/proto2/go/proto"
	"google.golang.org/grpc/status" /* copybara-comment */

	glog "github.com/golang/glog" /* copybara-comment */
)

// To create a HTTP handler from a gRPC handler:
//
//  func (h *FooHTTPHandler) GetFoo(w http.ResponseWriter, r *http.Request) {
// 	  req := &fpb.GetFooRequest{Name: r.RequestURI}
// 	  resp, err := fooServer.GetFoo(r.Context(), req)
//    if err != nil {
//      httputils.WriteError(w, err)
//    }
// 	  WriteResp(w, resp)
//  }

// WriteResp writes an protobuf message to the response.
func WriteResp(w http.ResponseWriter, m proto.Message) {
	WriteCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	// w.Header().Set("Cache-Control", "no-store")
	// w.Header().Set("Pragma", "no-cache")
	if err := EncodeJSONPB(w, m); err != nil {
		glog.Errorf("EncodeJSONPB() failed: %v", err)
		http.Error(w, "encoding the response failed", http.StatusInternalServerError)
		return
	}
}

// WriteNonProtoResp writes a reponse.
// For protobuf message responses use WriteResp(w, resp) instead.
func WriteNonProtoResp(w http.ResponseWriter, resp interface{}) {
	WriteCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	if err := EncodeJSON(w, resp); err != nil {
		glog.Errorf("EncodeJSON() failed: %v", err)
		http.Error(w, "encoding the response failed", http.StatusInternalServerError)
		return
	}
}

// WriteError writes an error to the response.
// Does nothing if status is nil.
func WriteError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	glog.InfoDepth(1, err)
	st := status.Convert(err)
	w.WriteHeader(HTTPStatus(st.Code()))
	WriteResp(w, st.Proto())
}

// WriteHTMLResp writes a "text/html" type string to the ResponseWriter.
func WriteHTMLResp(w http.ResponseWriter, b string) {
	WriteCorsHeaders(w)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(b))
}

// WriteRedirect writes a redirect to the provider URL.
// If the provided URL is relative, it will be relative to the request URL.
func WriteRedirect(w http.ResponseWriter, r *http.Request, redirect string) {
	// url = strings.ReplaceAll(url, "%2526", "&")
	// url = strings.ReplaceAll(url, "%253F", "?")
	WriteCorsHeaders(w)
	http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
}

// WriteCorsHeaders writes CORS headers (https://www.w3.org/TR/cors) to the response.
func WriteCorsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Origin, Accept, Authorization, X-Link-Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
}

// RedirectHTMLPage retuns the HTML page generated by http.Redirect.
// This is copied from http package.
func RedirectHTMLPage(dst string) string {
	return `<a href="` + HTMLReplacer.Replace(dst) + `">Temporary Redirect</a>.` + "\n\n"
}

// HTMLReplacer escape URL parameters for HTML.
// This is copied from http package.
var HTMLReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&#34;",
	"'", "&#39;",
)
