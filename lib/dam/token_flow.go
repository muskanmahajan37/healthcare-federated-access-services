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
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging" /* copybara-comment: logging */
	"google.golang.org/grpc/codes" /* copybara-comment */
	"google.golang.org/grpc/status" /* copybara-comment */
	"golang.org/x/oauth2" /* copybara-comment */
	"github.com/pborman/uuid" /* copybara-comment */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/adapter" /* copybara-comment: adapter */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/auditlog" /* copybara-comment: auditlog */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/ga4gh" /* copybara-comment: ga4gh */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/httputil" /* copybara-comment: httputil */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/hydra" /* copybara-comment: hydra */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/storage" /* copybara-comment: storage */
	"github.com/GoogleCloudPlatform/healthcare-federated-access-services/lib/timeutil" /* copybara-comment: timeutil */

	pb "github.com/GoogleCloudPlatform/healthcare-federated-access-services/proto/dam/v1" /* copybara-comment: go_proto */
)

const (
	maxResourceStateSeconds = 300
)

var (
	// resourcePathRE is for realm name, resource name, view name, role name, and interface name lookup only.
	resourcePathRE = regexp.MustCompile(`^([^\s/]*)/resources/([^\s/]+)/views/([^\s/]+)/roles/([^\s/]+)/interfaces/([^\s/]+)$`)
	// TODO: remove this older path when DDAP no longer uses it
	oldResourcePathRE = regexp.MustCompile(`^([^\s/]*)/resources/([^\s/]+)/views/([^\s/]+)/roles/([^\s/]+)$`)
)

func extractBearerToken(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if len(auth) == 0 {
		return "", fmt.Errorf("bearer token not found")
	}

	parts := strings.Split(auth, " ")
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return "", fmt.Errorf("token is not a bearer token")
	}

	return parts[1], nil
}

func extractAuthCode(r *http.Request) (string, error) {
	code := httputil.QueryParam(r, "code")
	if len(code) != 0 {
		return code, nil
	}
	return "", fmt.Errorf("auth code not found")
}

func parseTTL(maxAgeStr, ttlStr string) (time.Duration, error) {
	if len(maxAgeStr) > 0 {
		return timeutil.ParseSeconds(maxAgeStr)
	}
	if len(ttlStr) == 0 {
		return defaultTTL, nil
	}

	ttl, err := timeutil.ParseDuration(ttlStr)
	if err != nil {
		return 0, fmt.Errorf("TTL parameter %q format error: %v", ttlStr, err)
	}
	return ttl, nil
}

func extractTTL(maxAgeStr, ttlStr string) (time.Duration, error) {
	// TODO ttl params should remove.
	ttl, err := parseTTL(maxAgeStr, ttlStr)
	if err != nil {
		return 0, err
	}

	if ttl == 0 {
		return defaultTTL, nil
	}
	if ttl < 0 || ttl > maxTTL {
		return 0, fmt.Errorf("TTL parameter %q out of range: must be positive and not exceed %s", ttlStr, maxTTLStr)
	}
	return ttl, nil
}

func responseKeyFile(r *http.Request) bool {
	return httputil.QueryParam(r, "response_type") == "key-file-type"
}

func (s *Service) generateResourceToken(ctx context.Context, clientID, resourceName, viewName, role string, ttl time.Duration, useKeyFile bool, id *ga4gh.Identity, cfg *pb.DamConfig, res *pb.Resource, view *pb.View) (*pb.ResourceResults_ResourceAccess, int, error) {
	sRole, err := adapter.ResolveServiceRole(role, view, res, cfg)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !viewHasRole(view, role) {
		return nil, http.StatusBadRequest, fmt.Errorf("role %q is not defined on resource %q view %q", role, resourceName, viewName)
	}
	st, ok := cfg.ServiceTemplates[view.ServiceTemplate]
	if !ok {
		return nil, http.StatusInternalServerError, fmt.Errorf("view %q service template %q is not defined", viewName, view.ServiceTemplate)
	}
	adapt := s.adapters.ByServiceName[st.ServiceName]
	var aggregates []*adapter.AggregateView
	if adapt.IsAggregator() {
		aggregates, err = resolveAggregates(res, view, cfg, s.adapters)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}
	tokenFormat := ""
	if useKeyFile {
		tokenFormat = "application/json"
	}
	adapterAction := &adapter.Action{
		Aggregates:      aggregates,
		Identity:        id,
		Issuer:          s.getIssuerString(),
		ClientID:        clientID,
		Config:          cfg,
		GrantRole:       role,
		MaxTTL:          maxTTL,
		Resource:        res,
		ServiceRole:     sRole,
		ServiceTemplate: st,
		TTL:             ttl,
		View:            view,
		TokenFormat:     tokenFormat,
	}
	result, err := adapt.MintToken(ctx, adapterAction)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	if httputil.IsJSON(result.TokenFormat) && result.Credentials != nil {
		return &pb.ResourceResults_ResourceAccess{
			Credentials: map[string]string{"key_file": result.Credentials["json"]},
		}, http.StatusOK, nil
	}
	if useKeyFile {
		return nil, http.StatusBadRequest, fmt.Errorf("adapter cannot create key file format")
	}

	return &pb.ResourceResults_ResourceAccess{
		Credentials: result.Credentials,
		Labels:      result.Labels,
		ExpiresIn:   uint32(ttl.Seconds()),
	}, http.StatusOK, nil
}

func (s *Service) oauthConf(brokerName string, broker *pb.TrustedIssuer, clientSecret string, scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     broker.ClientId,
		ClientSecret: clientSecret,
		Scopes:       scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  broker.AuthUrl,
			TokenURL: broker.TokenUrl,
		},
		RedirectURL: s.domainURL + loggedInPath,
	}
}

func (s *Service) resourceViewRoleFromRequest(list []string) ([]resourceViewRole, error) {
	out := []resourceViewRole{}
	if len(list) == 0 {
		return nil, fmt.Errorf("resource parameter not found")
	}

	for _, res := range list {
		if !strings.HasPrefix(res, s.domainURL) {
			return nil, fmt.Errorf("requested resource %q not in this DAM", res)
		}
		prefix := s.domainURL + "/dam/"
		path := strings.ReplaceAll(res, prefix, "")

		m := resourcePathRE.FindStringSubmatch(path)
		if len(m) == 0 {
			// TODO: remove support for oldResourcePath
			m = oldResourcePathRE.FindStringSubmatch(path)
			if len(m) > 4 {
				m = append(m, "")
			}
		}

		if len(m) > 5 {
			out = append(out, resourceViewRole{realm: m[1], resource: m[2], view: m[3], role: m[4], interf: m[5], url: res})
			continue
		}
		return nil, fmt.Errorf("resource %q has invalid format", res)
	}

	return out, nil
}

type resourceViewRole struct {
	realm    string
	resource string
	view     string
	role     string
	interf   string
	url      string
}

type authHandlerIn struct {
	tokenType       pb.ResourceTokenRequestState_TokenType
	realm           string
	stateID         string
	redirect        string
	ttl             time.Duration
	clientID        string
	responseKeyFile bool
	resources       []resourceViewRole
	challenge       string
}

type authHandlerOut struct {
	oauth   *oauth2.Config
	stateID string
}

func (s *Service) auth(ctx context.Context, in authHandlerIn) (*authHandlerOut, int, error) {
	tx, err := s.store.Tx(true)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	defer tx.Finish()

	sec, err := s.loadSecrets(tx)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	realm := in.realm
	if in.tokenType == pb.ResourceTokenRequestState_DATASET {
		if len(in.resources) == 0 {
			return nil, http.StatusBadRequest, fmt.Errorf("empty resource list")
		}
		realm = in.resources[0].realm
	}

	cfg, err := s.loadConfig(tx, realm)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	broker, ok := cfg.TrustedIssuers[s.defaultBroker]
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("broker %q is not defined", s.defaultBroker)
	}
	clientSecret, ok := sec.GetBrokerSecrets()[broker.ClientId]
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("client secret of broker %q is not defined", s.defaultBroker)
	}

	var list []*pb.ResourceTokenRequestState_Resource

	for _, rvr := range in.resources {
		if rvr.realm != realm {
			return nil, http.StatusConflict, fmt.Errorf("cannot authorize resources using different realms")
		}

		resName := rvr.resource
		viewName := rvr.view
		roleName := rvr.role
		interf := rvr.interf
		if err := checkName(resName); err != nil {
			return nil, http.StatusBadRequest, err
		}
		if err := checkName(viewName); err != nil {
			return nil, http.StatusBadRequest, err
		}

		res, ok := cfg.Resources[resName]
		if !ok {
			return nil, http.StatusNotFound, fmt.Errorf("resource not found: %q", resName)
		}
		view, ok := res.Views[viewName]
		if !ok {
			return nil, http.StatusNotFound, fmt.Errorf("view %q not found for resource %q", viewName, resName)
		}
		grantRole := roleName
		if len(grantRole) == 0 {
			grantRole = view.DefaultRole
		}
		if !viewHasRole(view, grantRole) {
			return nil, http.StatusBadRequest, fmt.Errorf("role %q is not defined on resource %q view %q", grantRole, resName, viewName)
		}
		st, ok := cfg.ServiceTemplates[view.ServiceTemplate]
		if !ok {
			return nil, http.StatusInternalServerError, fmt.Errorf("service template %q is invalid for resource %q view %q", view.ServiceTemplate, resName, viewName)
		}
		// TODO: remove support for oldResourcePath
		if len(interf) == 0 {
			for k := range st.Interfaces {
				interf = k
				break
			}
		}
		if _, ok = st.Interfaces[interf]; !ok {
			return nil, http.StatusBadRequest, fmt.Errorf("interface %q is not defined on resource %q view %q service template %q", interf, resName, viewName, view.ServiceTemplate)
		}

		list = append(list, &pb.ResourceTokenRequestState_Resource{
			Realm:     rvr.realm,
			Resource:  resName,
			View:      viewName,
			Role:      grantRole,
			Interface: interf,
			Url:       rvr.url,
		})
	}

	// TODO: need support real policy filter
	scopes := []string{"openid", "ga4gh_passport_v1", "identities", "account_admin"}
	if in.tokenType == pb.ResourceTokenRequestState_ENDPOINT {
		scopes = []string{"openid", "identities"}
	}

	sID := uuid.New()

	state := &pb.ResourceTokenRequestState{
		Type:            in.tokenType,
		ClientId:        in.clientID,
		State:           in.stateID,
		Broker:          s.defaultBroker,
		Redirect:        in.redirect,
		Ttl:             int64(in.ttl),
		ResponseKeyFile: in.responseKeyFile,
		Resources:       list,
		Challenge:       in.challenge,
		EpochSeconds:    time.Now().Unix(),
		Realm:           realm,
	}

	err = s.store.WriteTx(storage.ResourceTokenRequestStateDataType, storage.DefaultRealm, storage.DefaultUser, sID, storage.LatestRev, state, nil, tx)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	conf := s.oauthConf(s.defaultBroker, broker, clientSecret, scopes)
	return &authHandlerOut{
		oauth:   conf,
		stateID: sID,
	}, http.StatusOK, nil
}

type loggedInHandlerIn struct {
	authCode string
	stateID  string
}

type loggedInHandlerOut struct {
	redirect   string
	stateID    string
	subject    string
	challenge  string
	identities []string
}

func (s *Service) loggedIn(ctx context.Context, in loggedInHandlerIn) (*loggedInHandlerOut, int, error) {
	tx, err := s.store.Tx(true)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	defer tx.Finish()

	state := &pb.ResourceTokenRequestState{}
	err = s.store.ReadTx(storage.ResourceTokenRequestStateDataType, storage.DefaultRealm, storage.DefaultUser, in.stateID, storage.LatestRev, state, tx)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	sec, err := s.loadSecrets(tx)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	realm := state.Realm
	cfg, err := s.loadConfig(tx, realm)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	broker, ok := cfg.TrustedIssuers[state.Broker]
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("unknown identity broker %q", state.Broker)
	}

	clientSecret, ok := sec.GetBrokerSecrets()[broker.ClientId]
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("client secret of broker %q is not defined", s.defaultBroker)
	}

	conf := s.oauthConf(state.Broker, broker, clientSecret, []string{})
	tok, err := conf.Exchange(ctx, in.authCode)
	if err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("token exchange failed. %s", err)
	}

	id, err := s.upstreamTokenToPassportIdentity(ctx, cfg, tx, tok.AccessToken, broker.ClientId)
	if err != nil {
		return nil, http.StatusUnauthorized, err
	}

	if state.Type == pb.ResourceTokenRequestState_DATASET {
		return s.loggedInForDatasetToken(ctx, id, state, cfg, in.stateID, realm, tx)
	}

	return s.loggedInForEndpointToken(id, state, in.stateID, tx)
}

func resourceToString(res *pb.ResourceTokenRequestState_Resource) string {
	return fmt.Sprintf("%s/%s/%s/%s", res.Realm, res.Resource, res.View, res.Role)
}

func writePolicyDeccisionLog(logger *logging.Client, id *ga4gh.Identity, res *pb.ResourceTokenRequestState_Resource, ttl time.Duration, err error) {
	log := &auditlog.PolicyDecisionLog{
		TokenID:       id.ID,
		TokenSubject:  id.Subject,
		TokenIssuer:   id.Issuer,
		Resource:      resourceToString(res),
		TTL:           timeutil.TTLString(ttl),
		PassAuthCheck: true,
	}

	if err != nil {
		log.PassAuthCheck = false
		log.Message = err.Error()
	}

	auditlog.WritePolicyDecisionLog(logger, log)
}

func (s *Service) loggedInForDatasetToken(ctx context.Context, id *ga4gh.Identity, state *pb.ResourceTokenRequestState, cfg *pb.DamConfig, stateID, realm string, tx storage.Tx) (*loggedInHandlerOut, int, error) {
	ttl := time.Duration(state.Ttl)

	list := state.Resources
	if len(list) == 0 {
		return nil, http.StatusInternalServerError, fmt.Errorf("empty resource list")
	}
	for _, r := range list {
		if r.Realm != realm {
			return nil, http.StatusConflict, fmt.Errorf("cannot authorize resources using different realms")
		}

		err := checkAuthorization(ctx, id, ttl, r.Resource, r.View, r.Role, cfg, state.ClientId, s.ValidateCfgOpts())
		writePolicyDeccisionLog(s.logger, id, r, ttl, err)
		if err != nil {
			return nil, httputil.FromError(err), err
		}
	}

	state.Issuer = id.Issuer
	state.Subject = id.Subject
	err := s.store.WriteTx(storage.ResourceTokenRequestStateDataType, storage.DefaultRealm, storage.DefaultUser, stateID, storage.LatestRev, state, nil, tx)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	return &loggedInHandlerOut{
		stateID:   stateID,
		subject:   id.Subject,
		challenge: state.Challenge,
	}, http.StatusOK, nil
}

func (s *Service) loggedInForEndpointToken(id *ga4gh.Identity, state *pb.ResourceTokenRequestState, stateID string, tx storage.Tx) (*loggedInHandlerOut, int, error) {
	err := s.store.DeleteTx(storage.ResourceTokenRequestStateDataType, storage.DefaultRealm, storage.DefaultUser, stateID, storage.LatestRev, tx)
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	identities := []string{id.Subject}
	for k := range id.Identities {
		identities = append(identities, k)
	}

	return &loggedInHandlerOut{
		subject:    id.Subject,
		challenge:  state.Challenge,
		identities: identities,
	}, http.StatusOK, nil
}

// ResourceTokens returns a set of access tokens for a set of resources.
func (s *Service) ResourceTokens(w http.ResponseWriter, r *http.Request) {
	tx, err := s.store.Tx(false)
	if err != nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, err)
		return
	}
	defer tx.Finish()

	auth, err := extractBearerToken(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err)
		return
	}

	cart := ""
	if s.useHydra {
		cart, err = s.extractCartFromAccessToken(auth)
		if err != nil {
			httputil.WriteRPCResp(w, nil, err)
			return
		}
	}

	state, id, err := s.resourceTokenState(cart, tx)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err)
		return
	}
	if len(state.Resources) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, fmt.Errorf("empty resource list"))
		return
	}
	cfg, err := s.loadConfig(tx, state.Resources[0].Realm)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err)
		return
	}

	ctx := r.Context()
	keyFile := false
	out := &pb.ResourceResults{
		Resources:    make(map[string]*pb.ResourceResults_ResourceDescriptor),
		Access:       make(map[string]*pb.ResourceResults_ResourceAccess),
		EpochSeconds: uint32(time.Now().Unix()),
	}
	for i, r := range state.Resources {
		res, ok := cfg.Resources[r.Resource]
		if !ok {
			httputil.WriteError(w, http.StatusNotFound, fmt.Errorf("resource not found: %q", r.Resource))
			return
		}

		view, ok := res.Views[r.View]
		if !ok {
			httputil.WriteError(w, http.StatusNotFound, fmt.Errorf("view %q not found for resource %q", r.View, r.Resource))
			return
		}

		result, status, err := s.generateResourceToken(ctx, state.ClientId, r.Resource, r.View, r.Role, time.Duration(state.Ttl), keyFile, id, cfg, res, view)
		if err != nil {
			httputil.WriteError(w, status, err)
			return
		}
		access := strconv.Itoa(i)

		interMap := map[string]*pb.ResourceResults_InterfaceEntry{}
		for k, v := range makeViewInterfaces(view, res, cfg, s.adapters) {
			entry := &pb.ResourceResults_InterfaceEntry{}
			interMap[k] = entry
			for _, uri := range v.Uri {
				entry.Items = append(entry.Items, &pb.ResourceResults_ResourceInterface{Uri: uri, Labels: v.Labels})
			}
		}

		out.Resources[r.Url] = &pb.ResourceResults_ResourceDescriptor{
			Interfaces:  interMap,
			Permissions: makeRoleCategories(view, r.Role, cfg),
			Access:      access,
		}
		out.Access[access] = &pb.ResourceResults_ResourceAccess{
			Credentials: result.Credentials,
			Labels:      result.Labels,
		}
	}
	httputil.WriteRPCResp(w, out, nil)
}

func (s *Service) resourceTokenState(stateID string, tx storage.Tx) (*pb.ResourceTokenRequestState, *ga4gh.Identity, error) {
	state := &pb.ResourceTokenRequestState{}
	err := s.store.ReadTx(storage.ResourceTokenRequestStateDataType, storage.DefaultRealm, storage.DefaultUser, stateID, storage.LatestRev, state, tx)
	if err != nil {
		return nil, nil, err
	}

	if len(state.Issuer) == 0 || len(state.Subject) == 0 {
		return nil, nil, fmt.Errorf("unauthorized")
	}

	now := time.Now().Unix()
	if now-state.EpochSeconds > maxResourceStateSeconds {
		return nil, nil, fmt.Errorf("authorization expired")
	}

	return state, &ga4gh.Identity{
		Issuer:  state.Issuer,
		Subject: state.Subject,
	}, nil
}

// LoggedInHandler implements endpoint "/loggedin" for broker auth code redirect.
func (s *Service) LoggedInHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	code, err := extractAuthCode(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err)
		return
	}

	stateID := httputil.QueryParam(r, "state")
	if len(stateID) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, fmt.Errorf("request must include state"))
	}

	out, st, err := s.loggedIn(r.Context(), loggedInHandlerIn{authCode: code, stateID: stateID})
	if err != nil {
		httputil.WriteError(w, st, err)
		return
	}

	if s.useHydra {
		ext := map[string]interface{}{}
		if len(out.identities) > 0 {
			ext["identities"] = out.identities
		}

		hydra.SendLoginSuccess(w, r, s.httpClient, s.hydraAdminURL, out.challenge, out.subject, out.stateID, ext)
		return
	}

	httputil.WriteStatus(w, status.New(codes.Unimplemented, "oidc service not supported"))
}
