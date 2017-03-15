/*
Copyright 2015 All rights reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/jose"
	"github.com/go-resty/resty"
	"github.com/labstack/echo/middleware"
	"github.com/stretchr/testify/assert"
)

func TestRolePermissionsMiddleware(t *testing.T) {
	cfg := newFakeKeycloakConfig()
	cfg.SkipTokenVerification = false
	cfg.Resources = []*Resource{
		{
			URL:     "/*",
			Methods: allHTTPMethods,
			Roles:   []string{fakeTestRole},
		},
		{
			URL:     fakeAdminRoleURL,
			Methods: allHTTPMethods,
			Roles:   []string{fakeAdminRole},
		},
		{
			URL:     fakeTestRoleURL,
			Methods: []string{"GET"},
			Roles:   []string{fakeTestRole},
		},
		{
			URL:     fakeTestAdminRolesURL,
			Methods: []string{"GET"},
			Roles:   []string{fakeAdminRole, fakeTestRole},
		},
		{
			URL:         fakeTestWhitelistedURL,
			WhiteListed: true,
			Methods:     []string{"GET"},
			Roles:       []string{},
		},
	}
	px, idp, svc := newTestProxyService(cfg)

	// test cases
	cs := []struct {
		URI       string
		Method    string
		Redirects bool
		HasToken  bool
		NotSigned bool
		Expires   time.Duration
		Roles     []string
		Expects   int
	}{
		{
			URI:     "/",
			Expects: http.StatusUnauthorized,
		},
		{ // check whitelisted is passed
			URI:     "/auth_all/white_listed/one",
			Expects: http.StatusOK,
		},
		{ // check for redirect
			URI:       "/",
			Redirects: true,
			Expects:   http.StatusTemporaryRedirect,
		},
		{
			URI:       "/oauth/callback",
			Redirects: true,
			Expects:   http.StatusBadRequest,
		},
		{
			URI:       "/oauth/health",
			Redirects: true,
			Expects:   http.StatusOK,
		},
		{ // check with a token
			URI:       "/",
			Redirects: false,
			HasToken:  true,
			Expects:   http.StatusForbidden,
		},
		{ // check with a token and wrong roles
			URI:       "/",
			Redirects: false,
			HasToken:  true,
			Roles:     []string{"one", "two"},
			Expects:   http.StatusForbidden,
		},
		{ // token, wrong roles
			URI:       fakeTestRoleURL,
			Redirects: false,
			HasToken:  true,
			Roles:     []string{"bad_role"},
			Expects:   http.StatusForbidden,
		},
		{ // token, wrong roles, no 'get' method
			URI:       fakeTestRoleURL,
			Method:    http.MethodPost,
			Redirects: false,
			HasToken:  true,
			Roles:     []string{"bad_role"},
			Expects:   http.StatusOK,
		},
		{ // check with correct token
			URI:       "/",
			Redirects: false,
			HasToken:  true,
			Roles:     []string{fakeTestRole},
			Expects:   http.StatusOK,
		},
		{ // check with correct token, not signed
			URI:       "/",
			Redirects: false,
			HasToken:  true,
			NotSigned: true,
			Roles:     []string{fakeTestRole},
			Expects:   http.StatusForbidden,
		},
		{ // check with correct token, signed
			URI:       fakeAdminRoleURL,
			Method:    http.MethodPost,
			Redirects: false,
			HasToken:  true,
			Roles:     []string{fakeTestRole},
			Expects:   http.StatusForbidden,
		},
		{ // check with correct token, signed, wrong roles
			URI:       fakeAdminRoleURL,
			Redirects: false,
			HasToken:  true,
			Roles:     []string{fakeTestRole},
			Expects:   http.StatusForbidden,
		},
		{ // check with correct token, signed, wrong roles
			URI:       fakeAdminRoleURL,
			Redirects: false,
			HasToken:  true,
			Roles:     []string{fakeTestRole, fakeAdminRole},
			Expects:   http.StatusOK,
		},
		{ // strange url
			URI:       fakeAdminRoleURL + "/.." + fakeAdminRoleURL,
			Redirects: false,
			Expects:   http.StatusUnauthorized,
		},
		{ // strange url, token
			URI:       fakeAdminRoleURL + "/.." + fakeAdminRoleURL,
			Redirects: false,
			HasToken:  true,
			Roles:     []string{"hehe"},
			Expects:   http.StatusForbidden,
		},
		{ // strange url, token
			URI:       "/test/../admin",
			Redirects: false,
			HasToken:  true,
			Expects:   http.StatusForbidden,
		},
		{ // strange url, token, role
			URI:       "/test/../admin",
			Redirects: false,
			HasToken:  true,
			Roles:     []string{fakeAdminRole},
			Expects:   http.StatusForbidden,
		},
		{ // strange url, token, wrong roles
			URI:       "/test/.." + fakeTestAdminRolesURL,
			Redirects: false,
			HasToken:  true,
			Roles:     []string{fakeAdminRole},
			Expects:   http.StatusForbidden,
		},
	}
	for i, c := range cs {
		px.config.NoRedirects = !c.Redirects
		// step: make the client
		hc := resty.New().SetRedirectPolicy(resty.NoRedirectPolicy())
		if c.HasToken {
			token := newTestToken(idp.getLocation())
			if len(c.Roles) > 0 {
				token.setRealmsRoles(c.Roles)
			}
			if c.Expires > 0 {
				token.setExpiration(time.Now().Add(c.Expires))
			}
			if !c.NotSigned {
				signed, err := idp.signToken(token.claims)
				if !assert.NoError(t, err, "case %d, unable to sign the token, error: %s", i, err) {
					continue
				}
				hc.SetAuthToken(signed.Encode())
			} else {
				jwt := token.getToken()
				hc.SetAuthToken(jwt.Encode())
			}
		}
		// step: make the request
		resp, err := hc.R().Execute(c.Method, svc+c.URI)
		if err != nil {
			if !strings.Contains(err.Error(), "Auto redirect is disable") {
				assert.NoError(t, err, "case %d, unable to make request, error: %s", i, err)
				continue
			}
		}
		// step: check against the expected
		assert.Equal(t, c.Expects, resp.StatusCode(), "case %d, uri: %s,  expected: %d, got: %d",
			i, c.URI, c.Expects, resp.StatusCode())
	}
}

func TestCrossSiteHandler(t *testing.T) {
	cases := []struct {
		Method  string
		Cors    middleware.CORSConfig
		Headers map[string]string
	}{
		{
			Method: http.MethodGet,
			Cors: middleware.CORSConfig{
				AllowOrigins: []string{"*"},
			},
			Headers: map[string]string{
				"Access-Control-Allow-Origin": "*",
			},
		},
		{
			Method: http.MethodGet,
			Cors: middleware.CORSConfig{
				AllowOrigins: []string{"*", "https://examples.com"},
			},
			Headers: map[string]string{
				"Access-Control-Allow-Origin": "*",
			},
		},
		{
			Method: http.MethodGet,
			Cors: middleware.CORSConfig{
				AllowOrigins: []string{"*"},
			},
			Headers: map[string]string{
				"Access-Control-Allow-Origin": "*",
			},
		},
		{
			Method: http.MethodOptions,
			Cors: middleware.CORSConfig{
				AllowOrigins: []string{"*"},
				AllowMethods: []string{"GET", "POST"},
			},
			Headers: map[string]string{
				"Access-Control-Allow-Origin":  "*",
				"Access-Control-Allow-Methods": "GET,POST",
			},
		},
	}

	for i, c := range cases {
		cfg := newFakeKeycloakConfig()
		// update the cors options
		cfg.NoRedirects = false
		cfg.CorsCredentials = c.Cors.AllowCredentials
		cfg.CorsExposedHeaders = c.Cors.ExposeHeaders
		cfg.CorsHeaders = c.Cors.AllowHeaders
		cfg.CorsMaxAge = time.Duration(time.Duration(c.Cors.MaxAge) * time.Second)
		cfg.CorsMethods = c.Cors.AllowMethods
		cfg.CorsOrigins = c.Cors.AllowOrigins
		// create the test service
		svc := newTestServiceWithConfig(cfg)
		// login and get a token
		token, err := makeTestOauthLogin(svc + fakeAuthAllURL)
		if err != nil {
			t.Errorf("case %d, unable to login to service, error: %s", i, err)
			continue
		}
		// make a request and check the response
		resp, err := resty.New().R().
			SetHeader("Content-Type", "application/json").
			SetAuthToken(token).
			Execute(c.Method, svc+fakeAuthAllURL)
		if !assert.NoError(t, err, "case %d, unable to make request, error: %s", i, err) {
			continue
		}
		if resp.StatusCode() < 200 || resp.StatusCode() > 300 {
			continue
		}
		// check the headers are present
		for k, v := range c.Headers {
			assert.NotEmpty(t, resp.Header().Get(k), "case %d did not find header: %s", i, k)
			assert.Equal(t, v, resp.Header().Get(k), "case %d expected: %s, got: %s", i, v, resp.Header().Get(k))
		}
	}
}

func TestCustomHeadersHandler(t *testing.T) {
	cs := []struct {
		Match   []string
		Claims  jose.Claims
		Expects map[string]string
	}{ /*
			{
				Match: []string{"subject", "userid", "email", "username"},
				Claims: jose.Claims{
					"id":    "test-subject",
					"name":  "rohith",
					"email": "gambol99@gmail.com",
				},
				Expects: map[string]string{
					"X-Auth-Subject":  "test-subject",
					"X-Auth-Userid":   "rohith",
					"X-Auth-Email":    "gambol99@gmail.com",
					"X-Auth-Username": "rohith",
				},
			},
			{
				Match: []string{"roles"},
				Claims: jose.Claims{
					"roles": []string{"a", "b", "c"},
				},
				Expects: map[string]string{
					"X-Auth-Roles": "a,b,c",
				},
			},*/
		{
			Match: []string{"given_name", "family_name"},
			Claims: jose.Claims{
				"email":              "gambol99@gmail.com",
				"name":               "Rohith Jayawardene",
				"family_name":        "Jayawardene",
				"preferred_username": "rjayawardene",
				"given_name":         "Rohith",
			},
			Expects: map[string]string{
				"X-Auth-Given-Name":  "Rohith",
				"X-Auth-Family-Name": "Jayawardene",
			},
		},
	}
	for i, x := range cs {
		cfg := newFakeKeycloakConfig()
		cfg.AddClaims = x.Match
		_, idp, svc := newTestProxyService(cfg)
		// create a token with those clams
		token := newTestToken(idp.getLocation())
		token.mergeClaims(x.Claims)
		signed, _ := idp.signToken(token.claims)
		// make the request
		var response testUpstreamResponse
		resp, err := resty.New().
			SetAuthToken(signed.Encode()).R().
			SetResult(&response).
			Get(svc + "/auth_all/test")

		if !assert.NoError(t, err, "case %d, unable to make the request, error: %s", i, err) {
			continue
		}

		// ensure the headers
		if !assert.Equal(t, http.StatusOK, resp.StatusCode(), "case %d, expected: %d, got: %d", i, http.StatusOK, resp.StatusCode()) {
			continue
		}

		for k, v := range x.Expects {
			assert.NotEmpty(t, response.Headers.Get(k), "case %d, did not have header: %s", i, k)
			assert.Equal(t, v, response.Headers.Get(k), "case %d, expected: %s, got: %s", i, v, response.Headers.Get(k))
		}
	}
}

func TestAdmissionHandlerRoles(t *testing.T) {
	cfg := newFakeKeycloakConfig()
	cfg.NoRedirects = true
	cfg.Resources = []*Resource{
		{
			URL:     "/admin",
			Methods: allHTTPMethods,
			Roles:   []string{"admin"},
		},
		{
			URL:     "/test",
			Methods: []string{"GET"},
			Roles:   []string{"test"},
		},
		{
			URL:     "/either",
			Methods: allHTTPMethods,
			Roles:   []string{"admin", "test"},
		},
		{
			URL:     "/",
			Methods: allHTTPMethods,
		},
	}
	_, idp, svc := newTestProxyService(cfg)
	cs := []struct {
		Method   string
		URL      string
		Roles    []string
		Expected int
	}{
		{
			URL:      "/admin",
			Roles:    []string{},
			Expected: http.StatusForbidden,
		},
		{
			URL:      "/admin",
			Roles:    []string{"admin"},
			Expected: http.StatusOK,
		},
		{
			URL:      "/test",
			Expected: http.StatusOK,
			Roles:    []string{"test"},
		},
		{
			URL:      "/either",
			Expected: http.StatusOK,
			Roles:    []string{"test", "admin"},
		},
		{
			URL:      "/either",
			Expected: http.StatusForbidden,
			Roles:    []string{"no_roles"},
		},
		{
			URL:      "/",
			Expected: http.StatusOK,
		},
	}

	for i, c := range cs {
		// step: create token from the toles
		token := newTestToken(idp.getLocation())
		if len(c.Roles) > 0 {
			token.setRealmsRoles(c.Roles)
		}
		jwt, err := idp.signToken(token.claims)
		if !assert.NoError(t, err) {
			continue
		}

		// step: make the request
		resp, err := resty.New().R().
			SetAuthToken(jwt.Encode()).
			Get(svc + c.URL)
		if !assert.NoError(t, err) {
			continue
		}
		assert.Equal(t, c.Expected, resp.StatusCode(), "case %d, expected: %d, got: %d",
			i, c.Expected, resp.StatusCode())
		if c.Expected == http.StatusOK {
			assert.NotEmpty(t, resp.Header().Get(testProxyAccepted), "case %d, not proxy header found", i)
		}
	}
}

func TestRolesAdmissionHandlerClaims(t *testing.T) {
	cfg := newFakeKeycloakConfig()
	cfg.NoRedirects = true
	cfg.Resources = []*Resource{
		{
			URL:     "/admin",
			Methods: allHTTPMethods,
		},
	}
	cs := []struct {
		Matches  map[string]string
		Claims   jose.Claims
		Expected int
	}{
		{
			Matches:  map[string]string{"cal": "test"},
			Claims:   jose.Claims{},
			Expected: http.StatusForbidden,
		},
		{
			Matches:  map[string]string{"item": "^tes$"},
			Claims:   jose.Claims{},
			Expected: http.StatusForbidden,
		},
		{
			Matches: map[string]string{"item": "^tes$"},
			Claims: jose.Claims{
				"item": "tes",
			},
			Expected: http.StatusOK,
		},
		{
			Matches: map[string]string{"item": "^test", "found": "something"},
			Claims: jose.Claims{
				"item": "test",
			},
			Expected: http.StatusForbidden,
		},
		{
			Matches: map[string]string{"item": "^test", "found": "something"},
			Claims: jose.Claims{
				"item":  "tester",
				"found": "something",
			},
			Expected: http.StatusOK,
		},
		{
			Matches: map[string]string{"item": ".*"},
			Claims: jose.Claims{
				"item": "test",
			},
			Expected: http.StatusOK,
		},
		{
			Matches:  map[string]string{"item": "^t.*$"},
			Claims:   jose.Claims{"item": "test"},
			Expected: http.StatusOK,
		},
	}

	for i, c := range cs {
		cfg.MatchClaims = c.Matches
		_, idp, svc := newTestProxyService(cfg)

		token := newTestToken(idp.getLocation())
		token.mergeClaims(c.Claims)
		jwt, err := idp.signToken(token.claims)
		if !assert.NoError(t, err) {
			continue
		}
		// step: inject a resource
		resp, err := resty.New().R().
			SetAuthToken(jwt.Encode()).
			Get(svc + "/admin")
		if !assert.NoError(t, err) {
			continue
		}
		assert.Equal(t, c.Expected, resp.StatusCode(), "case %d failed, expected: %d but got: %d", i, c.Expected, resp.StatusCode())
		if c.Expected == http.StatusOK {
			assert.NotEmpty(t, resp.Header().Get(testProxyAccepted))
		}
	}
}
