package plugin

import (
	"time"

	"github.com/mahendrapaipuri/authlib/authn"
	"github.com/mahendrapaipuri/authlib/authz"
	"github.com/mahendrapaipuri/authlib/cache"
	"github.com/mahendrapaipuri/grafana-dashboard-reporter-app/pkg/plugin/helpers"
)

// authzClientFor returns an authorization enforcement client for the frozen
// Grafana URL and token pair. Clients are cached per token so that a token
// rotation can never make an in-flight request silently use the client (and
// the identity) built for another request.
//
// All inputs come from a request snapshot and are immutable for the lifetime
// of the report.
func (app *App) authzClientFor(grafanaAppURL, token string) (authz.EnforcementClient, error) {
	// Prevent two concurrent calls from creating the same client
	app.mx.Lock()
	defer app.mx.Unlock()

	cacheKey := grafanaAppURL + "\x00" + token

	if client, ok := app.authzClients[cacheKey]; ok {
		app.ctxLogger.Debug("token unchanged returning existing authz client")

		return client, nil
	}

	// Header "typ" has been added only in Grafana 11.1.0 (https://github.com/grafana/grafana/pull/87430)
	// So this check will fail for Grafana < 11.1.0
	// Set VerifierConfig{DisableTypHeaderCheck: true} for those cases
	var disableTypHeaderCheck bool
	if helpers.SemverCompare(app.grafanaSemVer, "v11.1.0") == -1 {
		disableTypHeaderCheck = true
	}

	// Initialize the authorization client
	client, err := authz.NewEnforcementClient(authz.Config{
		APIURL: grafanaAppURL,
		Token:  token,
		// Grafana is signing the JWTs on local setups
		JWKsURL: grafanaAppURL + "/api/signing-keys/keys",
	},
		// Use the configured HTTP client
		authz.WithHTTPClient(app.httpClient),
		// Configure verifier
		authz.WithVerifier(authn.NewVerifier[authz.CustomClaims](authn.VerifierConfig{
			DisableTypHeaderCheck: disableTypHeaderCheck,
		},
			authn.TokenTypeID,
			authn.NewKeyRetriever(authn.KeyRetrieverConfig{
				SigningKeysURL: grafanaAppURL + "/api/signing-keys/keys",
			},
				authn.WithHTTPClientKeyRetrieverOpt(app.httpClient)),
		)),
		// Fetch all the user permission prefixed with dashboards
		authz.WithSearchByPrefix("dashboards"),
		// Fetch all the user permission prefixed with folders
		authz.WithSearchByPrefix("folders"),
		// Use a cache with a lower expiry time
		authz.WithCache(cache.NewLocalCache(cache.Config{
			Expiry:          10 * time.Second,
			CleanupInterval: 5 * time.Second,
		})),
	)
	if err != nil {
		return nil, err
	}

	app.authzClients[cacheKey] = client

	return client, nil
}
