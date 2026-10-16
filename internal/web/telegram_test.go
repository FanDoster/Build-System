package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/FanDoster/Build-System/internal/auth"
	"github.com/FanDoster/Build-System/internal/models"
)

func TestTelegramSettingsCheckboxAndConnectionStatus(t *testing.T) {
	database, _ := setup(t)
	h := New(database, "/builds")
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	p := &models.Project{Name: "telegram", RepoURL: "https://github.com/u/app", ImageName: "telegram"}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		p.TelegramNotifications = enabled
		if err := database.UpdateProject(p); err != nil {
			t.Fatal(err)
		}
		for _, connected := range []bool{false, true} {
			h.TelegramConfigured = func() bool { return connected }
			w := get(t, mux, fmt.Sprintf("/projects/%d/settings", p.ID))
			if w.Code != 200 {
				t.Fatalf("settings: %d %s", w.Code, w.Body)
			}
			body := w.Body.String()
			checked := strings.Contains(body, `name="telegram_notifications" checked`)
			if checked != enabled {
				t.Fatalf("checkbox checked = %v, want %v", checked, enabled)
			}
			for _, want := range []string{
				`type="checkbox" name="telegram_notifications"`, "Telegram notifications",
				"build results and related agent issues", "Uncheck to mute this project", "Email is unchanged",
				"The owner must create a Telegram group and add the notification bot", "no personal-chat fallback",
				fmt.Sprintf(`data-api-url="/builds/api/projects/%d"`, p.ID),
			} {
				if !strings.Contains(body, want) {
					t.Errorf("settings missing %q", want)
				}
			}
			wantStatus := "Telegram group not connected."
			if connected {
				wantStatus = "Telegram group connected."
			}
			if !strings.Contains(body, wantStatus) {
				t.Errorf("settings missing %q", wantStatus)
			}
		}
	}
	h.TelegramConfigured = nil
	body := get(t, mux, fmt.Sprintf("/projects/%d/settings", p.ID)).Body.String()
	if !strings.Contains(body, "Enabling this option will not deliver messages until setup is complete.") {
		t.Error("missing unconfigured warning")
	}
}

func TestTelegramSettingsRefuseAgentBeforeStatusCallback(t *testing.T) {
	database, _ := setup(t)
	h := New(database, "")
	called := 0
	h.TelegramConfigured = func() bool { called++; return true }
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	p := &models.Project{Name: "telegram", RepoURL: "https://github.com/u/app", ImageName: "telegram"}
	if err := database.CreateProject(p); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(database, "operator-password", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.SetAgentToken("agent-token")
	wrapped := a.Middleware(mux)
	for _, token := range []string{"agent-token", "operator-password"} {
		req := httptest.NewRequest("GET", fmt.Sprintf("/projects/%d/settings", p.ID), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, req)
		want := 403
		if token == "operator-password" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("%s settings: %d %s", token, w.Code, w.Body)
		}
		if token == "agent-token" && called != 0 {
			t.Error("agent ran connection callback")
		}
	}
	if called != 1 {
		t.Fatalf("operator callback calls = %d, want 1", called)
	}
}

func TestTelegramCheckboxSerializedFromCheckedState(t *testing.T) {
	_, mux := setup(t)
	script := get(t, mux, "/static/js/app.js").Body.String()
	if !strings.Contains(script, "telegram_notifications: f.telegram_notifications.checked") {
		t.Fatal("settings must serialize checked state, including unchecked false")
	}
	// The Telegram setting must not replace existing queue, polling or secret serialization.
	for _, want := range []string{"poll_enabled: f.poll_enabled.checked", "payload.executor_profiles = JSON.parse", "payload.webhook_secret", "payload.clone_token"} {
		if !strings.Contains(script, want) {
			t.Errorf("settings lost %q", want)
		}
	}
}

// Exercise the actual submit handler, not only the source string: unchecked
// checkboxes must send false rather than disappear as they do in FormData.
func TestTelegramCheckboxSubmitSendsBothBooleanValues(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script, err := staticFS.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	harness := `
const vm = require('node:vm');
const source = process.argv[1];
const start = source.indexOf('  function initSettingsForm() {');
const end = source.indexOf('  var MONTHS =', start);
if (start < 0 || end < 0) throw Error('settings handler not found');
for (const enabled of [true, false]) {
  let submit, sent;
  const field = value => ({value, checked: false, addEventListener() {}, focus() {}});
  const f = {};
  for (const name of ['name','repo_url','branch','dockerfile_path','image_name',
      'deploy_compose_path','deploy_service_name','build_command','build_path','upload_script',
      'webhook_secret','clone_token','new_queue']) f[name] = field('');
  f.name.value = 'game'; f.repo_url.value = 'https://github.com/u/game';
  f.branch.value = 'main'; f.dockerfile_path.value = 'Dockerfile'; f.image_name.value = 'game';
  f.executor = field('unity-windows');
  f.executor_profiles = field(JSON.stringify({linux: {build_command:'Build.Windows',build_path:'/output',upload_script:''}}));
  f.poll_interval_secs = field('90');
  f.poll_enabled = {checked: true}; f.no_cache = {checked: false};
  f.telegram_notifications = {checked: enabled, value: 'on'};
  const form = {elements: f, dataset: {apiUrl:'/builds/api/projects/7'},
    addEventListener(name, callback) {if (name === 'submit') submit = callback;}};
  const banner = {scrollIntoView() {}};
  const button = {addEventListener() {}};
  const ids = {'settings-form':form, 'settings-banner':banner, 'btn-save':button,
    'btn-delete':button, 'new-queue-field':{}};
  vm.runInNewContext(source.slice(start, end) + '\ninitSettingsForm();', {
    document: {getElementById(id) {return ids[id] || null;}},
    fetch(url, opts) {sent = {url, opts}; return Promise.resolve({ok:true});},
    setTimeout() {}, location: {}, confirm() {return false;}
  });
  submit({preventDefault() {}});
  if (!sent) throw Error('submit did not send request');
  const p = JSON.parse(sent.opts.body);
  if (p.telegram_notifications !== enabled) throw Error('wrong checkbox boolean: ' + sent.opts.body);
  if (p.poll_enabled !== true || p.executor !== 'unity-windows' || p.executor_profiles.linux.build_path !== '/output')
    throw Error('other settings lost');
  if ('webhook_secret' in p || 'clone_token' in p) throw Error('blank secrets overwrite existing values');
  if (sent.opts.headers['X-Builds-Csrf'] !== '1' || sent.opts.method !== 'PUT' || sent.url !== '/builds/api/projects/7')
    throw Error('request contract changed');
}
`
	out, err := exec.Command(node, "-e", harness, string(script)).CombinedOutput()
	if err != nil {
		t.Fatalf("checkbox serialization: %v\n%s", err, out)
	}
}
