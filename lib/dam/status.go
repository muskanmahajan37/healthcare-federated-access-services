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

package dam

import (
	"net/http"

	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/httputils" /* copybara-comment: httputils */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/storage" /* copybara-comment: storage */

	pb "github.com/GoogleCloudPlatform/healthcare-federated-access-services/proto/dam/v1" /* copybara-comment: go_proto */
)

func (s *Service) GetInfo(w http.ResponseWriter, r *http.Request) {
	out := &pb.GetInfoResponse{
		Name:      "Data Access Manager",
		Versions:  []string{"v1alpha"},
		StartTime: s.startTime,
	}
	realm := httputils.QueryParamWithDefault(r, "realm", storage.DefaultRealm)
	if cfg, err := s.loadConfig(nil, realm); err == nil {
		out.Ui = cfg.Ui
	}
	httputils.WriteResp(w, out)
}
