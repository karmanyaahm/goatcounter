// Copyright © 2019 Martin Tournoij – This file is part of GoatCounter and
// published under the terms of a slightly modified EUPL v1.2 license, which can
// be found in the LICENSE file or at https://license.goatcounter.com

package handlers

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"zgo.at/errors"
	"zgo.at/goatcounter"
	"zgo.at/goatcounter/bgrun"
	"zgo.at/goatcounter/cron"
	"zgo.at/guru"
	"zgo.at/zdb"
	"zgo.at/zhttp"
	"zgo.at/zhttp/header"
	"zgo.at/zvalidate"
)

type (
	apiError struct {
		Error  string              `json:"error,omitempty"`
		Errors map[string][]string `json:"errors,omitempty"`
	}
	authError struct {
		Error string `json:"error"`
	}
)

const respOK = `{"status":"ok"}`

type api struct{}

func (h api) mount(r chi.Router, db zdb.DB) {
	a := r.With(
		middleware.AllowContentType("application/json"),
		zhttp.Ratelimit(zhttp.RatelimitOptions{
			Client: zhttp.RatelimitIP,
			Store:  zhttp.NewRatelimitMemory(),
			Limit:  zhttp.RatelimitLimit(60, 120),
		}))

	a.Get("/api/v0/test", zhttp.Wrap(h.test))
	a.Post("/api/v0/test", zhttp.Wrap(h.test))

	a.Get("/api/v0/me", zhttp.Wrap(h.me))

	a.Post("/api/v0/export", zhttp.Wrap(h.export))
	a.Get("/api/v0/export/{id}", zhttp.Wrap(h.exportGet))
	a.Get("/api/v0/export/{id}/download", zhttp.Wrap(h.exportDownload))

	a.Post("/api/v0/count", zhttp.Wrap(h.count))

	// Note: DELETE not supported for sites and users intentionally, since it's
	// such a dangerous operation.
	a.Get("/api/v0/sites", zhttp.Wrap(h.siteList))
	a.Put("/api/v0/sites", zhttp.Wrap(h.siteCreate))
	a.Get("/api/v0/sites/{id}", zhttp.Wrap(h.siteGet))
	a.Post("/api/v0/sites/{id}", zhttp.Wrap(h.siteUpdate))  // Update all
	a.Patch("/api/v0/sites/{id}", zhttp.Wrap(h.siteUpdate)) // Update just fields given
}

func tokenFromHeader(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", guru.New(http.StatusForbidden, "no Authorization header")
	}
	b := strings.Fields(auth)
	if len(b) != 2 || b[0] != "Bearer" {
		return "", guru.New(http.StatusForbidden, "wrong format for Authorization header")
	}
	return b[1], nil
}

func (h api) auth(r *http.Request, perm goatcounter.APITokenPermissions) error {
	key, err := tokenFromHeader(r)
	if err != nil {
		return err
	}

	var token goatcounter.APIToken
	err = token.ByToken(r.Context(), key)
	if zdb.ErrNoRows(err) {
		return guru.New(http.StatusForbidden, "unknown token")
	}
	if err != nil {
		return err
	}

	var user goatcounter.User
	err = user.BySite(r.Context(), token.SiteID)
	if err != nil {
		return err
	}

	*r = *r.WithContext(goatcounter.WithUser(r.Context(), &user))

	var need []string
	if perm.Count && !token.Permissions.Count {
		need = append(need, "count")
	}
	if perm.Export && !token.Permissions.Export {
		need = append(need, "export")
	}
	if len(need) > 0 {
		return guru.Errorf(http.StatusForbidden, "requires %s permissions", need)
	}

	return nil
}

type apiExportRequest struct {
	// Pagination cursor; only export hits with an ID greater than this.
	StartFromHitID int64 `json:"start_from_hit_id"`
}

// For testing various generic properties about the API.
func (h api) test(w http.ResponseWriter, r *http.Request) error {
	var args struct {
		Perm     goatcounter.APITokenPermissions `json:"perm"`
		Status   int                             `json:"status"`
		Panic    bool                            `json:"panic"`
		Validate zvalidate.Validator             `json:"validate"`
	}

	_, err := zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	err = h.auth(r, args.Perm)
	if err != nil {
		return err
	}

	if args.Panic {
		panic("PANIC!")
	}

	if args.Validate.HasErrors() {
		return args.Validate
	}

	if args.Status != 0 {
		w.WriteHeader(args.Status)
		if args.Status == 500 {
			return errors.New("oh noes!")
		}
		return guru.Errorf(args.Status, "status %d", args.Status)
	}

	return zhttp.JSON(w, args)
}

type meResponse struct {
	User  goatcounter.User     `json:"user"`
	Token goatcounter.APIToken `json:"token"`
}

// GET /api/v0/me user
// Get information about the current user and API key.
//
// Response 200: meResponse
func (h api) me(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{})
	if err != nil {
		return err
	}

	key, err := tokenFromHeader(r)
	if err != nil {
		return err
	}
	var token goatcounter.APIToken
	err = token.ByToken(r.Context(), key)
	if err != nil {
		return err
	}

	u := goatcounter.GetUser(r.Context())
	return zhttp.JSON(w, meResponse{User: *u, Token: token})
}

// POST /api/v0/export export
// Start a new export in the background.
//
// This starts a new export in the background.
//
// Request body: apiExportRequest
// Response 202: zgo.at/goatcounter.Export
func (h api) export(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		Export: true,
	})
	if err != nil {
		return err
	}

	var req apiExportRequest
	_, err = zhttp.Decode(r, &req)
	if err != nil {
		return err
	}

	var export goatcounter.Export
	fp, err := export.Create(r.Context(), req.StartFromHitID)
	if err != nil {
		return err
	}

	ctx := goatcounter.NewContext(r.Context())
	bgrun.Run(fmt.Sprintf("export api:%d", export.SiteID), func() { export.Run(ctx, fp, false) })

	w.WriteHeader(http.StatusAccepted)
	return zhttp.JSON(w, export)
}

// GET /api/v0/export/{id} export
// Get details about an export.
//
// Response 200: zgo.at/goatcounter.Export
func (h api) exportGet(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		Export: true,
	})
	if err != nil {
		return err
	}

	v := zvalidate.New()
	id := v.Integer("id", chi.URLParam(r, "id"))
	if v.HasErrors() {
		return v
	}

	var export goatcounter.Export
	err = export.ByID(r.Context(), id)
	if err != nil {
		return err
	}

	return zhttp.JSON(w, export)
}

// GET /api/v0/export/{id}/download export
// Download an export file.
//
// Response 200 (text/csv): {data}
func (h api) exportDownload(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		Export: true,
	})
	if err != nil {
		return err
	}

	v := zvalidate.New()
	id := v.Integer("id", chi.URLParam(r, "id"))
	if v.HasErrors() {
		return v
	}

	var export goatcounter.Export
	err = export.ByID(r.Context(), id)
	if err != nil {
		return err
	}

	fp, err := os.Open(export.Path)
	if err != nil {
		if os.IsNotExist(err) {
			w.WriteHeader(404)
			return zhttp.JSON(w, apiError{Error: "it looks like there is no export yet"})
		}
		return err
	}
	defer fp.Close()

	err = header.SetContentDisposition(w.Header(), header.DispositionArgs{
		Type:     header.TypeAttachment,
		Filename: filepath.Base(export.Path),
	})
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/gzip")
	return zhttp.Stream(w, fp)
}

type APICountRequest struct {
	// Don't try to count unique visitors; every pageview will be considered a
	// "visit".
	NoSessions bool `json:"no_sessions"`

	// TODO: This is a new struct just because Kommentaar can't deal with it
	// otherwise... Need to rewrite a lot of that.

	Hits []APICountRequestHit `json:"hits"`
}

type APICountRequestHit struct {
	// Path of the pageview, or the event name. {required}
	Path string `json:"path"`

	// Page title, or some descriptive event title.
	Title string `json:"title"`

	// Is this an event?
	Event zdb.Bool `json:"event"`

	// Referrer value, can be an URL (i.e. the Referal: header) or any
	// string.
	Ref string `json:"ref"`

	// Screen size as "x,y,scaling"
	Size zdb.Floats `json:"size"`

	// Query parameters for this pageview, used to get campaign parameters.
	Query string `json:"query"`

	// Hint if this should be considered a bot; should be one of the JSBot*`
	// constants from isbot; note the backend may override this if it
	// detects a bot using another method.
	// https://github.com/zgoat/isbot/blob/master/isbot.go#L28
	Bot int `json:"bot"`

	// User-Agent header.
	UserAgent string `json:"user_agent"`

	// Location as ISO-3166-1 alpha2 string (e.g. NL, ID, etc.)
	Location string `json:"location"`

	// IP to get location from; not used if location is set. Also used for
	// session generation.
	IP string `json:"ip"`

	// Time this pageview should be recorded at; this can be in the past,
	// but not in the future.
	CreatedAt time.Time `json:"created_at"`

	// Normally a session is based on hash(User-Agent+IP+salt), but if you don't
	// send the IP address then we can't determine the session.
	//
	// In those cases, you can store your own session identifiers and send them
	// along. Note these will not be stored in the database as the sessionID
	// (just as the hashes aren't), they're just used as a unique grouping
	// identifier.
	//
	// You can also just disable sessions entirely with NoSessions.
	Session string `json:"session"`
}

// POST /api/v0/count count
// Count pageviews.
//
// This can count one or more pageviews. Pageviews are not persisted immediatly,
// but persisted in the background every 10 seconds.
//
// The maximum amount of pageviews per request is 100.
//
// Errors will have the key set to the index of the pageview. Any pageviews not
// listed have been processed and shouldn't be sent again.
//
// The response header has the X-Goatcounter-Memstore header set to the time of
// the last time pageviews were persisted to the database. This is useful if you
// want to (roughly) sync up with this.
//
// Request body: APICountRequest
// Response 202: {empty}
func (h api) count(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		Count: true,
	})
	if err != nil {
		return err
	}

	var args APICountRequest
	_, err = zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	if len(args.Hits) == 0 {
		w.WriteHeader(400)
		return zhttp.JSON(w, apiError{Error: "no hits"})
	}

	if len(args.Hits) > 100 {
		w.WriteHeader(400)
		return zhttp.JSON(w, apiError{Error: "maximum amount of pageviews in one batch is 100"})
	}

	errs := make(map[int]string)
	for i, a := range args.Hits {
		if a.Location == "" && a.IP != "" {
			a.Location = geo(a.IP)
		}

		hit := goatcounter.Hit{
			Path:       a.Path,
			Title:      a.Title,
			Ref:        a.Ref,
			Event:      a.Event,
			Size:       a.Size,
			Query:      a.Query,
			Bot:        a.Bot,
			CreatedAt:  a.CreatedAt,
			Browser:    a.UserAgent,
			Location:   a.Location,
			RemoteAddr: a.IP,
		}

		switch {
		case hit.Browser != "" && a.IP != "":
			// Handle as usual in memstore.
		case a.Session != "":
			hit.UserSessionID = a.Session
		case !args.NoSessions:
			errs[i] = "session or browser/IP not set; use no_sessions if you don't want to track unique visits"
			continue
		}

		hit.Defaults(r.Context())
		err = hit.Validate(r.Context())
		if err != nil {
			errs[i] = err.Error()
			continue
		}

		goatcounter.Memstore.Append(hit)
	}

	if len(errs) > 0 {
		w.WriteHeader(400)
		return zhttp.JSON(w, map[string]interface{}{
			"errors": errs,
		})
	}

	w.Header().Set("X-Goatcounter-Memstore", cron.LastMemstore.Get().Format(time.RFC3339Nano))
	w.WriteHeader(http.StatusAccepted)
	return zhttp.JSON(w, respOK)
}

type apiSitesResponse struct {
	Sites goatcounter.Sites `json:"sites"`
}

// GET /api/v0/sites sites
// List the current site and all additional sites.
//
// Response 200: apiSitesResponse
func (h api) siteList(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		SiteRead: true,
	})
	if err != nil {
		return err
	}

	sites := goatcounter.Sites{*goatcounter.MustGetSite(r.Context())}
	err = sites.ListSubs(r.Context())
	if err != nil {
		return err
	}

	return zhttp.JSON(w, apiSitesResponse{sites})
}

func (h api) siteFind(r *http.Request) (*goatcounter.Site, error) {
	v := zvalidate.New()
	id := v.Integer("id", chi.URLParam(r, "id"))
	if v.HasErrors() {
		return nil, v
	}

	var site goatcounter.Site
	err := site.ByID(r.Context(), id)
	if err != nil {
		return nil, err
	}

	siteID := Site(r.Context()).ID
	if !(site.ID == siteID || (site.Parent != nil && *site.Parent == siteID)) {
		return nil, guru.New(404, "")
	}

	return &site, nil
}

// GET /api/v0/sites/{id} sites
// Get information about a site.
//
// Get all information about one site.
//
// Response 200: goatcounter.Site
func (h api) siteGet(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		SiteRead: true,
	})
	if err != nil {
		return err
	}

	site, err := h.siteFind(r)
	if err != nil {
		return err
	}
	return zhttp.JSON(w, site)
}

// PUT /api/v0/sites sites
// Create a new site.
//
// Request body: goatcounter.Site
// Response 200: goatcounter.Site
func (h api) siteCreate(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		SiteCreate: true,
	})
	if err != nil {
		return err
	}

	var site goatcounter.Site
	_, err = zhttp.Decode(r, &site)
	if err != nil {
		return err
	}

	site.Parent = &Site(r.Context()).ID
	site.Plan = goatcounter.PlanChild
	err = site.Insert(r.Context())
	if err != nil {
		return err
	}

	return zhttp.JSON(w, site)
}

type apiSiteUpdateRequest struct {
	Settings   goatcounter.SiteSettings `json:"settings"`
	Cname      *string                  `json:"cname"`
	LinkDomain string                   `json:"link_domain"`
}

// POST /api/v0/sites/{id} sites
// PATCH /api/v0/sites/{id} sites
// Update a site.
//
// A POST request will *replace* the entire site with what's sent, blanking out
// any existing fields that may exist. A PATCH request will only update the
// fields that are sent.
//
// Request body: apiSiteUpdateRequest
// Response 200: goatcounter.Site
func (h api) siteUpdate(w http.ResponseWriter, r *http.Request) error {
	err := h.auth(r, goatcounter.APITokenPermissions{
		SiteUpdate: true,
	})
	if err != nil {
		return err
	}

	site, err := h.siteFind(r)
	if err != nil {
		return err
	}

	var args apiSiteUpdateRequest
	if r.Method == http.MethodPatch {
		args.LinkDomain = site.LinkDomain
		args.Cname = site.Cname
		args.Settings = site.Settings
	}

	_, err = zhttp.Decode(r, &args)
	if err != nil {
		return err
	}

	site.LinkDomain = args.LinkDomain
	site.Cname = args.Cname
	site.Settings = args.Settings
	err = site.Update(r.Context())
	if err != nil {
		return err
	}

	return zhttp.JSON(w, site)
}
