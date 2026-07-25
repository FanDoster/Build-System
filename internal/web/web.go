package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/FanDoster/Build-System/internal/auth"
	"github.com/FanDoster/Build-System/internal/db"
	"github.com/FanDoster/Build-System/internal/models"
)

// logText HTML-escapes log content and then encodes carriage returns as
// &#13;. The HTML tokenizer normalizes raw \r to \n BEFORE entity decoding,
// so this is the only way the client's CR-collapsing renderer can see the
// real \r bytes in server-rendered log text.
var tmplFuncs = template.FuncMap{
	"logText": func(s string) template.HTML {
		return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(s), "\r", "&#13;"))
	},
}

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Handler struct {
	DB       *db.DB
	BasePath string
}

func New(database *db.DB, basePath string) *Handler {
	return &Handler{DB: database, BasePath: basePath}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Static files
	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// Pages
	mux.HandleFunc("GET /", h.handleIndex)
	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("GET /projects/{id}", h.handleProject)
	mux.HandleFunc("GET /projects/{id}/settings", h.handleProjectSettings)
	mux.HandleFunc("GET /builds/{id}", h.handleBuild)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Already signed in (or auth disabled)? The login page is pointless.
	if auth.IsAuthed(r.Context()) {
		http.Redirect(w, r, h.BasePath+"/", http.StatusFound)
		return
	}
	h.render(w, r, "login", map[string]interface{}{
		"Title": "Sign in",
	})
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data map[string]interface{}) {
	tmpl, err := template.New(name).Funcs(tmplFuncs).ParseFS(templateFS, "templates/base.html", "templates/"+name+".html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if data == nil {
		data = map[string]interface{}{}
	}
	data["BasePath"] = h.BasePath
	data["Authed"] = auth.IsAuthed(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.ExecuteTemplate(w, "base", data)
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	projects, err := h.DB.ListProjects()
	if err != nil {
		http.Error(w, "failed to load projects: "+err.Error(), 500)
		return
	}
	builds, err := h.DB.ListRecentBuilds(10)
	if err != nil {
		http.Error(w, "failed to load builds: "+err.Error(), 500)
		return
	}
	h.render(w, r, "index", map[string]interface{}{
		"Title":    "Dashboard",
		"Projects": projects,
		"Builds":   builds,
	})
}

func (h *Handler) handleProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	project, err := h.DB.GetProject(id)
	if err != nil {
		http.Error(w, "project not found", 404)
		return
	}
	builds, err := h.DB.ListBuildsByProject(id, 0)
	if err != nil {
		http.Error(w, "failed to load builds: "+err.Error(), 500)
		return
	}
	h.render(w, r, "project", map[string]interface{}{
		"Title":   project.Name,
		"Project": project,
		"Builds":  builds,
	})
}

func (h *Handler) handleProjectSettings(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	project, err := h.DB.GetProject(id)
	if err != nil {
		http.Error(w, "project not found", 404)
		return
	}
	// Secrets are write-only in the UI: the page learns whether one is set,
	// never its value.
	hasWebhookSecret := project.WebhookSecret != ""
	hasCloneToken := project.CloneToken != ""
	project.Sanitize()
	if project.PollIntervalSecs == 0 {
		project.PollIntervalSecs = models.DefaultPollIntervalSecs
	}
	h.render(w, r, "settings", map[string]interface{}{
		"Title":               project.Name + " · Settings",
		"Project":             project,
		"HasWebhookSecret":    hasWebhookSecret,
		"HasCloneToken":       hasCloneToken,
		"MinPollIntervalSecs": models.MinPollIntervalSecs,
	})
}

func (h *Handler) handleBuild(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", 400)
		return
	}
	build, err := h.DB.GetBuild(id)
	if err != nil {
		http.Error(w, "build not found", 404)
		return
	}
	// Project may be gone if deleted after the build; the page degrades.
	project, _ := h.DB.GetProject(build.ProjectID)
	queuePos := 0
	if build.Status == models.StatusPending {
		queuePos, _ = h.DB.QueuePosition(build.ID)
	}
	h.render(w, r, "build", map[string]interface{}{
		"Title":    build.ProjectName + " #" + strconv.FormatInt(build.ID, 10),
		"Build":    build,
		"Project":  project,
		"QueuePos": queuePos,
	})
}
