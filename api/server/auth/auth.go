package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/micro/go-micro/v2/api/resolver"
	"github.com/micro/go-micro/v2/api/resolver/path"
	"github.com/micro/go-micro/v2/auth"
	"github.com/micro/go-micro/v2/logger"
	"golang.org/x/net/publicsuffix"
)

// CombinedAuthHandler wraps a server and authenticates requests
func CombinedAuthHandler(prefix, namespace string, r resolver.Resolver, h http.Handler) http.Handler {
	if r == nil {
		r = path.NewResolver()
	}

	return authHandler{
		handler:       h,
		resolver:      r,
		auth:          auth.DefaultAuth,
		servicePrefix: prefix,
		namespace:     namespace,
	}
}

type authHandler struct {
	handler       http.Handler
	auth          auth.Auth
	resolver      resolver.Resolver
	namespace     string
	servicePrefix string
}

func (h authHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Determine the namespace and set it in the header
	namespace := h.NamespaceFromRequest(req)
	req.Header.Set(auth.NamespaceKey, namespace)

	// Extract the token from the request
	var token string
	if header := req.Header.Get("Authorization"); len(header) > 0 {
		// Extract the auth token from the request
		if strings.HasPrefix(header, auth.BearerScheme) {
			token = header[len(auth.BearerScheme):]
		}
	} else {
		// Get the token out the cookies if not provided in headers
		if c, err := req.Cookie("micro-token"); err == nil && c != nil {
			token = strings.TrimPrefix(c.Value, auth.TokenCookieName+"=")
			req.Header.Set("Authorization", auth.BearerScheme+token)
		}
	}

	// Get the account using the token, fallback to a blank account
	// since some endpoints can be unauthenticated, so the lack of an
	// account doesn't necesserially mean a forbidden request
	acc, err := h.auth.Inspect(token)
	if err != nil {
		acc = &auth.Account{Namespace: namespace}
	}

	// Check the accounts namespace matches the namespace we're operating
	// within. If not forbid the request and log the occurance.
	if acc.Namespace != namespace {
		logger.Debugf("Cross namespace request warning: account %v (%v) requested access to %v in the %v namespace", acc.ID, acc.Namespace, req.URL.Path, namespace)
		// http.Error(w, "Forbidden namespace", 403)
	}

	// Determine the name of the service being requested
	endpoint, err := h.resolver.Resolve(req)
	if err == resolver.ErrInvalidPath || err == resolver.ErrNotFound {
		// a file not served by the resolver has been requested (e.g. favicon.ico)
		endpoint = &resolver.Endpoint{Path: req.URL.Path}
	} else if err != nil {
		logger.Error(err)
		http.Error(w, err.Error(), 500)
		return
	} else {
		// set the endpoint in the context so it can be used to resolve
		// the request later
		ctx := context.WithValue(req.Context(), resolver.Endpoint{}, endpoint)
		*req = *req.Clone(ctx)
	}

	// construct the resource name, e.g. home => go.micro.web.home
	resName := h.servicePrefix
	if len(endpoint.Name) > 0 {
		resName = resName + "." + endpoint.Name
	}

	// determine the resource path. there is an inconsistency in how resolvers
	// use method, some use it as Users.ReadUser (the rpc method), and others
	// use it as the HTTP method, e.g GET. TODO: Refactor this to make it consistent.
	resEndpoint := endpoint.Path
	if len(endpoint.Path) == 0 {
		resEndpoint = endpoint.Method
	}

	// Perform the verification check to see if the account has access to
	// the resource they're requesting
	res := &auth.Resource{Type: "service", Name: resName, Endpoint: resEndpoint, Namespace: namespace}
	if err := h.auth.Verify(acc, res); err == nil {
		// The account has the necessary permissions to access the resource
		h.handler.ServeHTTP(w, req)
		return
	}

	// The account is set, but they don't have enough permissions, hence
	// we return a forbidden error.
	if len(acc.ID) > 0 {
		http.Error(w, "Forbidden request", 403)
		return
	}

	// If there is no auth login url set, 401
	loginURL := h.auth.Options().LoginURL
	if loginURL == "" {
		http.Error(w, "unauthorized request", 401)
		return
	}

	// Redirect to the login path
	params := url.Values{"redirect_to": {req.URL.Path}}
	loginWithRedirect := fmt.Sprintf("%v?%v", loginURL, params.Encode())
	http.Redirect(w, req, loginWithRedirect, http.StatusTemporaryRedirect)
}

func (h authHandler) NamespaceFromRequest(req *http.Request) string {
	// check to see what the provided namespace is, we only do
	// domain mapping if the namespace is set to 'domain'
	if h.namespace != "domain" {
		return h.namespace
	}

	// determine the host, e.g. dev.micro.mu:8080
	host := req.URL.Hostname()
	if len(host) == 0 {
		if h, _, err := net.SplitHostPort(req.Host); err == nil {
			host = h // host does contain a port
		} else if strings.Contains(err.Error(), "missing port in address") {
			host = req.Host // host does not contain a port
		}
	}

	// check for an ip address
	if net.ParseIP(host) != nil {
		return auth.DefaultNamespace
	}

	// check for dev enviroment
	if host == "localhost" || host == "127.0.0.1" {
		return auth.DefaultNamespace
	}

	// extract the top level domain plus one (e.g. 'myapp.com')
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		logger.Debugf("Unable to extract domain from %v", host)
		return auth.DefaultNamespace
	}

	// check to see if the domain matches the host of micro.mu, in
	// these cases we return the default namespace
	if domain == host || domain == "micro.mu" {
		return auth.DefaultNamespace
	}

	// remove the domain from the host, leaving the subdomain
	subdomain := strings.TrimSuffix(host, "."+domain)

	// return the reversed subdomain as the namespace
	comps := strings.Split(subdomain, ".")
	for i := len(comps)/2 - 1; i >= 0; i-- {
		opp := len(comps) - 1 - i
		comps[i], comps[opp] = comps[opp], comps[i]
	}
	return strings.Join(comps, ".")
}
