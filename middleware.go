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
	"fmt"
	"regexp"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/go-oidc/jose"
	"github.com/labstack/echo"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/unrolled/secure"
)

const (
	// cxEnforce is the tag name for a request requiring
	cxEnforce = "Enforcing"
)

// loggingMiddleware is a custom http logger
func (r *oauthProxy) loggingMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(cx echo.Context) error {
			start := time.Now()
			next(cx)
			latency := time.Now().Sub(start)

			log.WithFields(log.Fields{
				"client_ip": cx.RealIP(),
				"method":    cx.Request().Method,
				"status":    cx.Response().Status,
				"bytes":     cx.Response().Size,
				"path":      cx.Request().URL.Path,
				"latency":   latency.String(),
			}).Infof("[%d] |%s| |%10v| %-5s %s", cx.Response().Status, cx.RealIP(), latency, cx.Request().Method, cx.Request().URL.Path)

			return nil
		}
	}
}

// metricsMiddleware is responsible for collecting metrics
func (r *oauthProxy) metricsMiddleware() echo.MiddlewareFunc {
	log.Infof("enabled the service metrics middleware, available on %s%s", oauthURL, metricsURL)

	statusMetrics := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_request_total",
			Help: "The HTTP requests partitioned by status code",
		},
		[]string{"code", "method"},
	)

	// step: register the metric with prometheus
	prometheus.MustRegisterOrGet(statusMetrics)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(cx echo.Context) error {
			statusMetrics.WithLabelValues(fmt.Sprintf("%d", cx.Response().Status), cx.Request().Method).Inc()
			return next(cx)
		}
	}
}

// authenticationMiddleware is responsible for verifying the access token
func (r *oauthProxy) authenticationMiddleware(resource *Resource) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(cx echo.Context) error {
			clientIP := cx.RealIP()

			// step: grab the user identity from the request
			user, err := r.getIdentity(cx.Request())
			if err != nil {
				log.WithFields(log.Fields{
					"error": err.Error(),
				}).Errorf("no session found in request, redirecting for authorization")

				return r.redirectToAuthorization(cx)
			}
			// step: inject the user into the context
			cx.Set(userContextName, user)

			// step: skip if we are running skip-token-verification
			if r.config.SkipTokenVerification {
				log.Warnf("skip token verification enabled, skipping verification - TESTING ONLY")

				if user.isExpired() {
					log.WithFields(log.Fields{
						"client_ip":  clientIP,
						"username":   user.name,
						"expired_on": user.expiresAt.String(),
					}).Errorf("the session has expired and verification switch off")

					return r.redirectToAuthorization(cx)
				}
			} else {
				if err := verifyToken(r.client, user.token); err != nil {
					// step: if the error post verification is anything other than a token
					// expired error we immediately throw an access forbidden - as there is
					// something messed up in the token
					if err != ErrAccessTokenExpired {
						log.WithFields(log.Fields{
							"client_ip": clientIP,
							"error":     err.Error(),
						}).Errorf("access token failed verification")

						return r.accessForbidden(cx)
					}

					// step: check if we are refreshing the access tokens and if not re-auth
					if !r.config.EnableRefreshTokens {
						log.WithFields(log.Fields{
							"client_ip":  clientIP,
							"email":      user.name,
							"expired_on": user.expiresAt.String(),
						}).Errorf("session expired and access token refreshing is disabled")

						return r.redirectToAuthorization(cx)
					}

					log.WithFields(log.Fields{
						"client_ip": clientIP,
						"email":     user.email,
					}).Infof("accces token for user has expired, attemping to refresh the token")

					// step: check if the user has refresh token
					refresh, err := r.retrieveRefreshToken(cx.Request(), user)
					if err != nil {
						log.WithFields(log.Fields{
							"client_ip": clientIP,
							"email":     user.email,
							"error":     err.Error(),
						}).Errorf("unable to find a refresh token for user")

						return r.redirectToAuthorization(cx)
					}

					// attempt to refresh the access token
					token, _, err := getRefreshedToken(r.client, refresh)
					if err != nil {
						switch err {
						case ErrRefreshTokenExpired:
							log.WithFields(log.Fields{
								"client_ip": clientIP,
								"email":     user.email,
							}).Warningf("refresh token has expired, cannot retrieve access token")

							r.clearAllCookies(cx.Request(), cx.Response().Writer)
						default:
							log.WithFields(log.Fields{"error": err.Error()}).Errorf("failed to refresh the access token")
						}

						return r.redirectToAuthorization(cx)
					}
					// get the expiration of the new access token
					expiresIn := r.getAccessCookieExpiration(token, refresh)

					log.WithFields(log.Fields{
						"client_ip":   clientIP,
						"cookie_name": r.config.CookieAccessName,
						"email":       user.email,
						"expires_in":  expiresIn.String(),
					}).Infof("injecting the refreshed access token cookie")

					// step: inject the refreshed access token
					r.dropAccessTokenCookie(cx.Request(), cx.Response().Writer, token.Encode(), expiresIn)

					if r.useStore() {
						go func(old, new jose.JWT, state string) {
							if err := r.DeleteRefreshToken(old); err != nil {
								log.WithFields(log.Fields{"error": err.Error()}).Errorf("failed to remove old token")
							}
							if err := r.StoreRefreshToken(new, state); err != nil {
								log.WithFields(log.Fields{"error": err.Error()}).Errorf("failed to store refresh token")
								return
							}
						}(user.token, token, refresh)
					}
					// step: update the with the new access token
					user.token = token
					// step: inject the user into the context
					cx.Set(userContextName, user)
				}
			}
			return next(cx)
		}
	}
}

// admissionMiddleware is responsible checking the access token against the protected resource
func (r *oauthProxy) admissionMiddleware(resource *Resource) echo.MiddlewareFunc {
	claimMatches := make(map[string]*regexp.Regexp, 0)
	for k, v := range r.config.MatchClaims {
		claimMatches[k] = regexp.MustCompile(v)
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(cx echo.Context) error {
			user := cx.Get(userContextName).(*userContext)

			// step: check the audience for the token is us
			if r.config.ClientID != "" && !user.isAudience(r.config.ClientID) {
				log.WithFields(log.Fields{
					"client_id":  r.config.ClientID,
					"email":      user.email,
					"expired_on": user.expiresAt.String(),
					"issuer":     user.audience,
				}).Warnf("access token audience is not us, redirecting back for authentication")

				return r.accessForbidden(cx)
			}

			// step: we need to check the roles
			if roles := len(resource.Roles); roles > 0 {
				if !hasRoles(resource.Roles, user.roles) {
					log.WithFields(log.Fields{
						"access":   "denied",
						"email":    user.email,
						"resource": resource.URL,
						"required": resource.getRoles(),
					}).Warnf("access denied, invalid roles")

					return r.accessForbidden(cx)
				}
			}

			// step: if we have any claim matching, lets validate the tokens has the claims
			for claimName, match := range claimMatches {
				// step: if the claim is NOT in the token, we access deny
				value, found, err := user.claims.StringClaim(claimName)
				if err != nil {
					log.WithFields(log.Fields{
						"access":   "denied",
						"email":    user.email,
						"resource": resource.URL,
						"error":    err.Error(),
					}).Errorf("unable to extract the claim from token")

					return r.accessForbidden(cx)
				}

				if !found {
					log.WithFields(log.Fields{
						"access":   "denied",
						"claim":    claimName,
						"email":    user.email,
						"resource": resource.URL,
					}).Warnf("the token does not have the claim")

					return r.accessForbidden(cx)
				}

				// step: check the claim is the same
				if !match.MatchString(value) {
					log.WithFields(log.Fields{
						"access":   "denied",
						"claim":    claimName,
						"email":    user.email,
						"issued":   value,
						"required": match,
						"resource": resource.URL,
					}).Warnf("the token claims does not match claim requirement")

					return r.accessForbidden(cx)
				}
			}

			log.WithFields(log.Fields{
				"access":   "permitted",
				"email":    user.email,
				"expires":  user.expiresAt.Sub(time.Now()).String(),
				"resource": resource.URL,
			}).Debugf("access permitted to resource")

			return next(cx)
		}
	}
}

// headersMiddleware is responsible for add the authentication headers for the upstream
func (r *oauthProxy) headersMiddleware(custom []string) echo.MiddlewareFunc {
	customClaims := make(map[string]string)
	for _, x := range custom {
		customClaims[x] = fmt.Sprintf("X-Auth-%s", toHeader(x))
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(cx echo.Context) error {
			// step: add any custom headers to the request
			for k, v := range r.config.Headers {
				cx.Request().Header.Set(k, v)
			}

			// step: retrieve the user context if any
			if user := cx.Get(userContextName); user != nil {
				id := user.(*userContext)
				cx.Request().Header.Set("X-Auth-Email", id.email)
				cx.Request().Header.Set("X-Auth-ExpiresIn", id.expiresAt.String())
				cx.Request().Header.Set("X-Auth-Roles", strings.Join(id.roles, ","))
				cx.Request().Header.Set("X-Auth-Subject", id.id)
				cx.Request().Header.Set("X-Auth-Token", id.token.Encode())
				cx.Request().Header.Set("X-Auth-Userid", id.name)
				cx.Request().Header.Set("X-Auth-Username", id.name)

				// step: add the authorization header if requested
				if r.config.EnableAuthorizationHeader {
					cx.Request().Header.Set("Authorization", fmt.Sprintf("Bearer %s", id.token.Encode()))
				}

				// step: inject any custom claims
				for claim, header := range customClaims {
					if claim, found := id.claims[claim]; found {
						cx.Request().Header.Set(header, fmt.Sprintf("%v", claim))
					}
				}
			}
			return next(cx)
		}
	}
}

// securityMiddleware performs numerous security checks on the request
func (r *oauthProxy) securityMiddleware() echo.MiddlewareFunc {
	log.Info("enabling the security filter middleware")
	// step: create the security options
	secure := secure.New(secure.Options{
		AllowedHosts:          r.config.Hostnames,
		BrowserXssFilter:      r.config.EnableBrowserXSSFilter,
		ContentSecurityPolicy: r.config.ContentSecurityPolicy,
		ContentTypeNosniff:    r.config.EnableContentNoSniff,
		FrameDeny:             r.config.EnableFrameDeny,
		SSLRedirect:           r.config.EnableHTTPSRedirect,
	})

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(cx echo.Context) error {
			if err := secure.Process(cx.Response().Writer, cx.Request()); err != nil {
				log.WithFields(log.Fields{"error": err.Error()}).Errorf("failed security middleware")
				return r.accessForbidden(cx)
			}
			return next(cx)
		}
	}
}
