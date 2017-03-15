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
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"path"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/go-oidc/oauth2"
	"github.com/labstack/echo"
)

// getRedirectionURL returns the redirectionURL for the oauth flow
func (r *oauthProxy) getRedirectionURL(cx echo.Context) string {
	var redirect string
	switch r.config.RedirectionURL {
	case "":
		// need to determine the scheme, cx.Request.URL.Scheme doesn't have it, best way is to default
		// and then check for TLS
		scheme := "http"
		if !cx.IsTLS() {
			scheme = "https"
		}
		// @QUESTION: should I use the X-Forwarded-<header>?? ..
		redirect = fmt.Sprintf("%s://%s",
			defaultTo(cx.Request().Header.Get("X-Forwarded-Proto"), scheme),
			defaultTo(cx.Request().Header.Get("X-Forwarded-Host"), cx.Request().Host))
	default:
		redirect = r.config.RedirectionURL
	}

	return fmt.Sprintf("%s/oauth/callback", redirect)
}

// oauthAuthorizationHandler is responsible for performing the redirection to oauth provider
func (r *oauthProxy) oauthAuthorizationHandler(cx echo.Context) error {
	// step: we can skip all of this if were not verifying the token
	if r.config.SkipTokenVerification {
		return cx.NoContent(http.StatusNotAcceptable)
	}
	// step: create a oauth client
	client, err := r.getOAuthClient(r.getRedirectionURL(cx))
	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
		}).Errorf("failed to retrieve the oauth client for authorization")

		return cx.NoContent(http.StatusInternalServerError)
	}

	// step: set the access type of the session
	var accessType string
	if containedIn("offline", r.config.Scopes) {
		accessType = "offline"
	}

	authURL := client.AuthCodeURL(cx.QueryParam("state"), accessType, "")

	log.WithFields(log.Fields{
		"access_type": accessType,
		"auth-url":    authURL,
		"client_ip":   cx.RealIP(),
	}).Debugf("incoming authorization request from client address: %s", cx.RealIP())

	// step: if we have a custom sign in page, lets display that
	if r.config.hasCustomSignInPage() {
		model := make(map[string]string, 0)
		model["redirect"] = authURL

		return cx.Render(http.StatusOK, path.Base(r.config.SignInPage), mergeMaps(model, r.config.Tags))
	}

	return r.redirectToURL(authURL, cx)
}

// oauthCallbackHandler is responsible for handling the response from oauth service
func (r *oauthProxy) oauthCallbackHandler(cx echo.Context) error {
	// step: is token verification switched on?
	if r.config.SkipTokenVerification {
		return cx.NoContent(http.StatusNotAcceptable)
	}
	// step: ensure we have a authorization code to exchange
	code := cx.QueryParam("code")
	if code == "" {
		return cx.NoContent(http.StatusBadRequest)
	}

	// step: create a oauth client
	client, err := r.getOAuthClient(r.getRedirectionURL(cx))
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to create a oauth2 client")

		return cx.NoContent(http.StatusInternalServerError)
	}

	// step: exchange the authorization for a access token
	resp, err := exchangeAuthenticationCode(client, code)
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to exchange code for access token")

		return r.accessForbidden(cx)
	}

	// step: parse decode the identity token
	token, identity, err := parseToken(resp.IDToken)
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to parse id token for identity")

		return r.accessForbidden(cx)
	}

	// step: verify the token is valid
	if err = verifyToken(r.client, token); err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to verify the id token")

		return r.accessForbidden(cx)
	}

	// step: attempt to decode the access token else we default to the id token
	access, id, err := parseToken(resp.AccessToken)
	if err != nil {
		log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to parse the access token, using id token only")
	} else {
		token = access
		identity = id
	}

	log.WithFields(log.Fields{
		"email":    identity.Email,
		"expires":  identity.ExpiresAt.Format(time.RFC3339),
		"duration": identity.ExpiresAt.Sub(time.Now()).String(),
	}).Infof("issuing access token for user, email: %s", identity.Email)

	// step: does the response has a refresh token and we are NOT ignore refresh tokens?
	if r.config.EnableRefreshTokens && resp.RefreshToken != "" {
		// step: encrypt the refresh token
		encrypted, err := encodeText(resp.RefreshToken, r.config.EncryptionKey)
		if err != nil {
			log.WithFields(log.Fields{"error": err.Error()}).Errorf("failed to encrypt the refresh token")

			return cx.NoContent(http.StatusInternalServerError)
		}

		// drop in the access token - cookie expiration = access token
		r.dropAccessTokenCookie(cx.Request(), cx.Response().Writer, token.Encode(),
			r.getAccessCookieExpiration(token, resp.RefreshToken))

		switch r.useStore() {
		case true:
			if err := r.StoreRefreshToken(token, encrypted); err != nil {
				log.WithFields(log.Fields{"error": err.Error()}).Warnf("failed to save the refresh token in the store")
			}
		default:
			// notes: not all idp refresh tokens are readable, google for example, so we attempt to decode into
			// a jwt and if possible extract the expiration, else we default to 10 days
			if _, ident, err := parseToken(resp.RefreshToken); err != nil {
				r.dropRefreshTokenCookie(cx.Request(), cx.Response().Writer, encrypted, time.Duration(240)*time.Hour)
			} else {
				r.dropRefreshTokenCookie(cx.Request(), cx.Response().Writer, encrypted, ident.ExpiresAt.Sub(time.Now()))
			}
		}
	} else {
		r.dropAccessTokenCookie(cx.Request(), cx.Response().Writer, token.Encode(), identity.ExpiresAt.Sub(time.Now()))
	}

	// step: decode the state variable
	state := "/"
	if cx.QueryParam("state") != "" {
		decoded, err := base64.StdEncoding.DecodeString(cx.QueryParam("state"))
		if err != nil {
			log.WithFields(log.Fields{
				"state": cx.QueryParam("state"),
				"error": err.Error(),
			}).Warnf("unable to decode the state parameter")
		} else {
			state = string(decoded)
		}
	}

	return r.redirectToURL(state, cx)
}

// loginHandler provide's a generic endpoint for clients to perform a user_credentials login to the provider
func (r *oauthProxy) loginHandler(cx echo.Context) error {
	errorMsg, code, err := func() (string, int, error) {
		// step: check if the handler is disable
		if !r.config.EnableLoginHandler {
			return "attempt to login when login handler is disabled", http.StatusNotImplemented, errors.New("login handler disabled")
		}

		// step: parse the client credentials
		username := cx.Request().PostFormValue("username")
		password := cx.Request().PostFormValue("password")
		if username == "" || password == "" {
			return "request does not have both username and password", http.StatusBadRequest, errors.New("no credentials")
		}

		// step: get the client
		client, err := r.client.OAuthClient()
		if err != nil {
			return "unable to create the oauth client for user_credentials request", http.StatusInternalServerError, err
		}

		token, err := client.UserCredsToken(username, password)
		if err != nil {
			if strings.HasPrefix(err.Error(), oauth2.ErrorInvalidGrant) {
				return "invalid user credentials provided", http.StatusUnauthorized, err
			}
			return "unable to request the access token via grant_type 'password'", http.StatusInternalServerError, err
		}

		// step: parse the token
		_, identity, err := parseToken(token.AccessToken)
		if err != nil {
			return "unable to decode the access token", http.StatusNotImplemented, err
		}

		r.dropAccessTokenCookie(cx.Request(), cx.Response().Writer, token.AccessToken, identity.ExpiresAt.Sub(time.Now()))

		cx.JSON(http.StatusOK, tokenResponse{
			IDToken:      token.IDToken,
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			ExpiresIn:    token.Expires,
			Scope:        token.Scope,
		})

		return "", http.StatusOK, nil
	}()
	if err != nil {
		log.WithFields(log.Fields{
			"client_ip": cx.RealIP(),
			"error":     err.Error,
		}).Errorf(errorMsg)

		return cx.NoContent(code)
	}

	return nil
}

// emptyHandler is responsible for doing nothing
func emptyHandler(cx echo.Context) error {
	return nil
}

//
// logoutHandler performs a logout
//  - if it's just a access token, the cookie is deleted
//  - if the user has a refresh token, the token is invalidated by the provider
//  - optionally, the user can be redirected by to a url
//
func (r *oauthProxy) logoutHandler(cx echo.Context) error {
	// the user can specify a url to redirect the back
	redirectURL := cx.QueryParam("redirect")

	// step: drop the access token
	user, err := r.getIdentity(cx.Request())
	if err != nil {
		return cx.NoContent(http.StatusBadRequest)
	}

	// step: can either use the id token or the refresh token
	identityToken := user.token.Encode()
	if refresh, err := r.retrieveRefreshToken(cx.Request(), user); err == nil {
		identityToken = refresh
	}
	r.clearAllCookies(cx.Request(), cx.Response().Writer)

	// step: check if the user has a state session and if so, revoke it
	if r.useStore() {
		go func() {
			if err := r.DeleteRefreshToken(user.token); err != nil {
				log.WithFields(log.Fields{
					"error": err.Error(),
				}).Errorf("unable to remove the refresh token from store")
			}
		}()
	}

	// step: get the revocation endpoint from either the idp and or the user config
	revocationURL := defaultTo(r.config.RevocationEndpoint, r.idp.EndSessionEndpoint.String())

	// step: do we have a revocation endpoint?
	if revocationURL != "" {
		client, err := r.client.OAuthClient()
		if err != nil {
			log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to retrieve the openid client")

			return cx.NoContent(http.StatusInternalServerError)
		}

		// step: add the authentication headers
		// @TODO need to add the authenticated request to go-oidc
		encodedID := url.QueryEscape(r.config.ClientID)
		encodedSecret := url.QueryEscape(r.config.ClientSecret)

		// step: construct the url for revocation
		request, err := http.NewRequest(http.MethodPost, revocationURL,
			bytes.NewBufferString(fmt.Sprintf("refresh_token=%s", identityToken)))
		if err != nil {
			log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to construct the revocation request")

			return cx.NoContent(http.StatusInternalServerError)
		}

		// step: add the authentication headers and content-type
		request.SetBasicAuth(encodedID, encodedSecret)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		// step: attempt to make the
		response, err := client.HttpClient().Do(request)
		if err != nil {
			log.WithFields(log.Fields{"error": err.Error()}).Errorf("unable to post to revocation endpoint")
			return nil
		}

		// step: add a log for debugging
		switch response.StatusCode {
		case http.StatusNoContent:
			log.WithFields(log.Fields{
				"user": user.email,
			}).Infof("successfully logged out of the endpoint")
		default:
			content, _ := ioutil.ReadAll(response.Body)
			log.WithFields(log.Fields{
				"status":   response.StatusCode,
				"response": fmt.Sprintf("%s", content),
			}).Errorf("invalid response from revocation endpoint")
		}
	}

	// step: should we redirect the user
	if redirectURL != "" {
		return r.redirectToURL(redirectURL, cx)
	}

	return cx.NoContent(http.StatusOK)
}

// expirationHandler checks if the token has expired
func (r *oauthProxy) expirationHandler(cx echo.Context) error {
	// step: get the access token from the request
	user, err := r.getIdentity(cx.Request())
	if err != nil {
		return cx.NoContent(http.StatusUnauthorized)
	}
	// step: check the access is not expired
	if user.isExpired() {
		return cx.NoContent(http.StatusUnauthorized)
	}

	return cx.NoContent(http.StatusOK)
}

// tokenHandler display access token to screen
func (r *oauthProxy) tokenHandler(cx echo.Context) error {
	user, err := r.getIdentity(cx.Request())
	if err != nil {
		return cx.String(http.StatusBadRequest, fmt.Sprintf("unable to retrieve session, error: %s", err))
	}
	cx.Response().Writer.Header().Set("Content-Type", "application/json")

	return cx.String(http.StatusOK, fmt.Sprintf("%s", user.token.Payload))
}

// healthHandler is a health check handler for the service
func (r *oauthProxy) healthHandler(cx echo.Context) error {
	cx.Response().Writer.Header().Set(versionHeader, version)

	return cx.String(http.StatusOK, "OK\n")
}

// debugHandler is responsible for providing the pprof
func (r *oauthProxy) debugHandler(cx echo.Context) error {
	name := cx.Param("name")
	switch cx.Request().Method {
	case http.MethodGet:
		switch name {
		case "heap":
			fallthrough
		case "goroutine":
			fallthrough
		case "block":
			fallthrough
		case "threadcreate":
			pprof.Handler(name).ServeHTTP(cx.Response().Writer, cx.Request())
		case "cmdline":
			pprof.Cmdline(cx.Response().Writer, cx.Request())
		case "profile":
			pprof.Profile(cx.Response().Writer, cx.Request())
		case "trace":
			pprof.Trace(cx.Response().Writer, cx.Request())
		case "symbol":
			pprof.Symbol(cx.Response().Writer, cx.Request())
		default:
			cx.NoContent(http.StatusNotFound)
		}
	case http.MethodPost:
		switch name {
		case "symbol":
			pprof.Symbol(cx.Response().Writer, cx.Request())
		default:
			cx.NoContent(http.StatusNotFound)
		}
	}

	return nil
}

// metricsHandler forwards the request into the prometheus handler
func (r *oauthProxy) metricsHandler(cx echo.Context) error {
	if r.config.LocalhostMetrics {
		if !net.ParseIP(cx.RealIP()).IsLoopback() {
			return r.accessForbidden(cx)
		}
	}
	r.prometheusHandler.ServeHTTP(cx.Response().Writer, cx.Request())

	return nil
}

// retrieveRefreshToken retrieves the refresh token from store or cookie
func (r *oauthProxy) retrieveRefreshToken(req *http.Request, user *userContext) (string, error) {
	var token string
	var err error

	// step: get the refresh token from the store or cookie
	switch r.useStore() {
	case true:
		token, err = r.GetRefreshToken(user.token)
	default:
		token, err = r.getRefreshTokenFromCookie(req)
	}
	if err != nil {
		return "", err
	}

	return decodeText(token, r.config.EncryptionKey)
}
