// Package plg mounts the PLG Customer Success Portal onto csm-portal's backend.
//
// ONE CALL IS ALL csm-portal's main.go NEEDS. Everything PLG requires — its
// config, its entity-service client, its services, its handlers, its identity
// middleware and its 26 routes — is assembled here. The merge adds files; it
// does not edit csm-portal's own.
//
// WHAT CHANGED FROM THE STANDALONE BFF:
//
//   - No server of its own. PLG's handlers mount on csm-portal's mux and run
//     inside its middleware chain: CORS, correlation IDs, logging, security
//     headers and, crucially, JWT validation are all csm-portal's.
//   - No X-PLG-User header. See middleware/identity.go — the caller's email now
//     comes from the validated token, so there is no header to spoof.
//   - No /plg/health. csm-portal has /health; a second liveness endpoint
//     answering for one application inside a shared service is a probe that lies.
//
// The queue poller comes across unchanged and runs as a goroutine for the
// lifetime of the process, which is a thing csm-portal's backend did not
// previously have. It is off unless configured.
package plg

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/config"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/entityclient"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/handler"
	plgmw "github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/middleware"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/queue"
	"github.com/wso2-open-operations/cs-tools/apps/csm-portal/backend/internal/plg/service"
)

// Mount wires PLG onto mux and returns a shutdown function for the poller.
//
// cfgPath points at PLG's own config.json — or nothing, in which case every
// setting comes from the PLG_* environment variables. Choreo deploys from
// environment alone, so the file is optional by design.
//
// A nil error with a nil shutdown means PLG is configured off.
func Mount(
	ctx context.Context,
	mux *http.ServeMux,
	cfgPath string,
	entityDefaults config.EntityDefaults,
) (func(), error) {
	cfg, err := config.LoadWith(cfgPath, entityDefaults)
	if err != nil {
		return nil, fmt.Errorf("plg: %w", err)
	}

	// One client, shared. It holds a connection pool of its own (net/http's)
	// and a timeout, so building a second would mean two.
	//
	// AUTHENTICATED, because entity-service sits behind a gateway that wants a
	// token. PLG presents the same OAuth2 client-credentials application as
	// every other upstream client in this backend — csm-portal's own entity
	// client, updates and SCIM all authenticate as it too. The credentials are
	// inherited rather than configured: see config.EntityDefaults.
	timeout := time.Duration(cfg.Entity.TimeoutSeconds) * time.Second
	entity := entityclient.New(entityclient.Config{
		BaseURL:    cfg.Entity.BaseURL,
		Timeout:    timeout,
		HTTPClient: entityHTTPClient(cfg.Entity.OAuth, timeout),
	})

	// Loaded before the listener opens: a malformed map is a startup failure,
	// not something to discover on the first webhook delivery.
	sourceMap, err := config.LoadSourceMap(cfg.Ingest.SourceMapPath)
	if err != nil {
		return nil, fmt.Errorf("plg: %w", err)
	}
	fields, platforms, attributes := sourceMap.Counts()
	slog.Info("plg: source map loaded",
		"fields", fields, "platformNames", platforms, "extraAttributes", attributes)

	handlers := handler.NewHandlers(
		service.NewReferenceService(entityclient.ReferenceRepo{Client: entity}),
		service.NewOrganizationService(entityclient.OrganizationRepo{Client: entity}),
		service.NewOrgPlatformService(entityclient.OrgPlatformRepo{Client: entity}),
		service.NewPlaybookService(entityclient.PlaybookRepo{Client: entity}),
		service.NewAnalyticsService(entityclient.AnalyticsRepo{Client: entity}),
		service.NewIngestService(entityclient.IngestRepo{Client: entity}, sourceMap),
		cfg.Ingest.SharedSecret,
	)

	// PLG's routes carry one extra middleware of their own: the identity
	// resolver, which turns the validated caller into the "user".id every PLG
	// write records. It wraps only this subtree — csm-portal's routes neither
	// need it nor pay for it.
	identity := plgmw.ResolveIdentity(entity)
	register(mux, handlers, identity)

	// The registration feed. The source publishes to the webhook-queue service;
	// the portal consumes from it here and lands each registration through
	// entity-service, which owns the transaction.
	if !cfg.Queue.Enabled {
		slog.Info("plg: queue poller disabled", "reason", "queue.enabled is false")
		return func() {}, nil
	}

	pollerCtx, stopPoller := context.WithCancel(ctx)
	done := make(chan struct{})
	poller := queue.NewPoller(
		cfg.Queue,
		service.NewIngestService(entityclient.IngestRepo{Client: entity}, sourceMap),
		entity,
	)
	go func() {
		defer close(done)
		poller.Run(pollerCtx)
	}()
	slog.Info("plg: queue poller started",
		"consumeURL", cfg.Queue.ConsumeURL, "intervalSeconds", cfg.Queue.PollIntervalSeconds)

	return func() {
		stopPoller()
		<-done
	}, nil
}

// register mounts PLG's routes, each wrapped in the identity middleware.
//
// Every path is prefixed /plg — csm-portal's backend has 116 routes of its own
// and `/products` means a different thing to each side. Verified before the
// merge: zero path overlaps.
func register(mux *http.ServeMux, h *handler.Handlers, identity func(http.Handler) http.Handler) {
	add := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, identity(fn))
	}

	// Reference data.
	add("GET /plg/me", h.Me)
	add("GET /plg/products", h.ListProducts)
	add("GET /plg/cs-users", h.ListCSUsers)
	add("GET /plg/lifecycle", h.Lifecycle)

	// Organisations and the overview tab.
	add("POST /plg/organizations/search", h.SearchOrganizations)
	add("GET /plg/organizations/{organizationId}", h.GetOrganization)
	add("PATCH /plg/organizations/{organizationId}", h.PatchOrganization)

	// The product tab — one organisation, one platform.
	add("GET /plg/organizations/{organizationId}/products/{product}", h.GetProduct)
	add("PATCH /plg/organizations/{organizationId}/products/{product}", h.PatchProduct)
	add("POST /plg/organizations/{organizationId}/products/{product}/playbook-runs", h.AttachPlaybook)
	add("POST /plg/organizations/{organizationId}/products/{product}/notes", h.CreateNote)

	// Playbook execution.
	add("DELETE /plg/playbook-runs/{playbookRunId}", h.DetachRun)
	add("PATCH /plg/playbook-run-tasks/{taskId}", h.PatchRunTask)
	add("PATCH /plg/notes/{noteId}", h.PatchNote)

	// New registrations.
	add("POST /plg/registrations/search", h.SearchRegistrations)
	add("POST /plg/registrations/{orgPlatformId}/acknowledge", h.Acknowledge)

	// Playbook manager.
	add("GET /plg/playbooks", h.ListPlaybooks)
	add("POST /plg/products/{product}/playbooks", h.CreatePlaybook)
	add("GET /plg/playbooks/{playbookId}", h.GetPlaybook)
	add("PATCH /plg/playbooks/{playbookId}", h.PatchPlaybook)
	add("PUT /plg/playbooks/{playbookId}/tasks", h.ReplacePlaybookTasks)
	add("DELETE /plg/playbooks/{playbookId}", h.DeletePlaybook)

	// Analytics.
	add("GET /plg/analytics/dashboard", h.Dashboard)
	add("GET /plg/work-queue", h.WorkQueue)

	// The registration feed. Machine-to-machine: no end-user identity, a shared
	// secret checked in the handler instead — so these deliberately skip the
	// identity middleware, which would reject every delivery.
	mux.HandleFunc("POST /plg/webhooks/registrations", h.Register)
	mux.HandleFunc("POST /plg/webhooks/registration", h.Register)
}

// entityHTTPClient returns a client that attaches an OAuth2 bearer token to
// every request, or nil when no grant is configured.
//
// Nil is the right answer for a local stack where entity-service is reachable
// without one; it is the wrong answer anywhere the gateway is in front, and
// there the symptom is a 401 from every PLG request rather than a startup
// failure — the grant is optional precisely so local development needs no
// credentials.
//
// Tokens are cached and refreshed by the oauth2 library. This is a second token
// source alongside csm-portal's own, which costs one extra token fetch per
// expiry window and keeps the two halves independently configurable.
func entityHTTPClient(cfg config.OAuth2Config, timeout time.Duration) *http.Client {
	if !cfg.Enabled() {
		return nil
	}
	cc := clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		TokenURL:     cfg.TokenURL,
	}
	if cfg.Scope != "" {
		cc.Scopes = strings.Split(cfg.Scope, ",")
	}
	tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient,
		&http.Client{Timeout: timeout})
	client := cc.Client(tokenCtx)
	client.Timeout = timeout
	return client
}
