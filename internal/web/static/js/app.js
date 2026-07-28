// Build-System — client JS.
// Build page: live SSE log streaming (offset-polling fallback), terminal-style
// renderer (CR collapse, ANSI SGR, line numbers/anchors, follow mode, search),
// step rail, in-place status updates, cancel/re-run actions.
// List pages: live build list over a WebSocket to /api/live (GET-polling
// fallback) — see initListLive.
// All URLs come from data-* attributes so BUILDS_BASE_PATH always works;
// offsets are server-computed bytes and are only ever echoed back.
(function () {
  'use strict';

  document.addEventListener('DOMContentLoaded', function () {
    renderAbsTimes();
    initTriggerButton();
    initBuildPage();
    initListLive();
    initSettingsForm();
    initLoginForm();
    initSignOut();
  });

  // --- Login page ---
  function initLoginForm() {
    var form = document.getElementById('login-form');
    if (!form) return;
    var banner = document.getElementById('login-banner');
    var btn = document.getElementById('btn-login');
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      btn.disabled = true;
      fetch(form.dataset.apiUrl, {
        method: 'POST',
        headers: { 'X-Builds-Csrf': '1', 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: form.elements.password.value }),
      })
        .then(function (res) {
          if (res.ok) {
            var base = document.body.dataset.basePath || '';
            var next = new URLSearchParams(location.search).get('next') || '/';
            // Only same-site relative paths — never protocol-relative or absolute.
            if (next.charAt(0) !== '/' || next.charAt(1) === '/') next = '/';
            location.href = base + next;
            return null;
          }
          return res.json().then(function (d) { throw new Error(d.error || ('HTTP ' + res.status)); });
        })
        .catch(function (err) {
          banner.hidden = false;
          banner.className = 'form-banner form-banner--err';
          banner.textContent = err.message;
          btn.disabled = false;
          form.elements.password.select();
        });
    });
  }

  function initSignOut() {
    var btn = document.getElementById('btn-signout');
    if (!btn) return;
    btn.addEventListener('click', function () {
      fetch(btn.dataset.url, { method: 'POST', headers: { 'X-Builds-Csrf': '1' } })
        .then(function () { location.href = btn.dataset.loginUrl; })
        .catch(function () { location.href = btn.dataset.loginUrl; });
    });
  }

  // --- Project settings page ---
  function initSettingsForm() {
    var form = document.getElementById('settings-form');
    if (!form) return;
    var banner = document.getElementById('settings-banner');
    var btnSave = document.getElementById('btn-save');
    var btnDelete = document.getElementById('btn-delete');

    function showBanner(kind, msg) {
      banner.hidden = false;
      banner.className = 'form-banner form-banner--' + kind;
      banner.textContent = msg;
      banner.scrollIntoView({ block: 'nearest' });
    }

    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var f = form.elements;
      var payload = {
        name: f.name.value.trim(),
        repo_url: f.repo_url.value.trim(),
        branch: f.branch.value.trim(),
        dockerfile_path: f.dockerfile_path.value.trim(),
        image_name: f.image_name.value.trim(),
        deploy_compose_path: f.deploy_compose_path.value.trim(),
        deploy_service_name: f.deploy_service_name.value.trim(),
        no_cache: f.no_cache.checked,
        poll_enabled: f.poll_enabled.checked,
        poll_interval_secs: parseInt(f.poll_interval_secs.value, 10),
        executor: f.executor.value.trim(),
      };
      if (!isFinite(payload.poll_interval_secs)) {
        showBanner('err', 'Poll interval must be a number.');
        return;
      }
      // Secrets are write-only: blank means "keep", the explicit clear
      // checkbox sends an empty string, a typed value replaces.
      var clearWebhook = document.getElementById('clear-webhook');
      if (clearWebhook && clearWebhook.checked) payload.webhook_secret = '';
      else if (f.webhook_secret.value) payload.webhook_secret = f.webhook_secret.value;
      var clearToken = document.getElementById('clear-token');
      if (clearToken && clearToken.checked) payload.clone_token = '';
      else if (f.clone_token.value) payload.clone_token = f.clone_token.value;

      btnSave.disabled = true;
      btnSave.textContent = 'Saving…';
      fetch(form.dataset.apiUrl, {
        method: 'PUT',
        headers: { 'X-Builds-Csrf': '1', 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      })
        .then(function (res) {
          if (res.ok) {
            showBanner('ok', 'Settings saved.');
            // Reload so secret set/unset badges and the header reflect reality.
            setTimeout(function () { location.reload(); }, 700);
            return null;
          }
          return res.json().then(function (d) { throw new Error(d.error || ('HTTP ' + res.status)); });
        })
        .catch(function (err) {
          showBanner('err', 'Save failed: ' + err.message);
          btnSave.disabled = false;
          btnSave.textContent = 'Save settings';
        });
    });

    btnDelete.addEventListener('click', function () {
      var name = form.dataset.projectName;
      if (!confirm('Delete project "' + name + '" and its entire build history? This cannot be undone.')) return;
      btnDelete.disabled = true;
      fetch(form.dataset.apiUrl, { method: 'DELETE', headers: { 'X-Builds-Csrf': '1' } })
        .then(function (res) {
          if (!res.ok) throw new Error('HTTP ' + res.status);
          location.href = form.dataset.homeUrl;
        })
        .catch(function (err) {
          showBanner('err', 'Delete failed: ' + err.message);
          btnDelete.disabled = false;
        });
    });
  }

  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  function pad2(n) { return (n < 10 ? '0' : '') + n; }

  // Render [data-abs] <time> elements (server emits UTC) in the viewer's
  // local timezone. Same-year dates drop the year; the full timestamp goes
  // in the title.
  function renderAbsTimes() {
    var els = document.querySelectorAll('time[data-abs]');
    for (var i = 0; i < els.length; i++) setAbsLabel(els[i]);
  }

  // setAbsLabel formats one [data-abs] <time> from its datetime attribute.
  // Also used for rows the live feed adds after load.
  function setAbsLabel(el) {
    var dt = el.getAttribute('datetime');
    if (!dt) return;
    var d = new Date(dt);
    if (isNaN(d.getTime())) return;
    var label = MONTHS[d.getMonth()] + ' ' + pad2(d.getDate()) + ' ' +
      pad2(d.getHours()) + ':' + pad2(d.getMinutes());
    if (d.getFullYear() !== new Date().getFullYear()) label = d.getFullYear() + ' ' + label;
    el.textContent = label;
    el.title = d.toLocaleString();
  }

  // Shared: compact duration like "40s" / "1m40s" / "1h05m".
  function fmtShort(sec) {
    sec = Math.max(0, Math.round(sec));
    if (sec < 60) return sec + 's';
    var m = Math.floor(sec / 60), s = sec % 60;
    if (m < 60) return m + 'm' + (s < 10 ? '0' : '') + s + 's';
    var h = Math.floor(m / 60);
    m = m % 60;
    return h + 'h' + (m < 10 ? '0' : '') + m + 'm';
  }

  var STEP_NAMES = { clone: 'Clone', checkout: 'Checkout', build: 'Build', push: 'Push', deploy: 'Deploy' };

  // --- Dashboard / project pages: live build list ---
  //
  // Transport: a WebSocket to /api/live, which pushes a whole snapshot of the
  // recent builds whenever anything changes (see internal/live). Snapshots are
  // idempotent, so applying the newest one is always correct — nothing to
  // resume, nothing to replay, and a reconnect needs no state. That is why a
  // build STARTED AFTER PAGE LOAD shows up: the list is reconciled against the
  // feed, not just refreshed in place.
  //
  // The same URL answers a plain GET with the same payload, which is the
  // fallback when the socket cannot be established (a reverse proxy that does
  // not forward Upgrade must degrade, not go dark).
  //
  // Elapsed times and progress bars animate locally from the absolute
  // timestamps in the snapshot, so the server only speaks when state changes.
  function initListLive() {
    var lists = Array.prototype.slice.call(document.querySelectorAll('[data-live-list]'));
    if (!lists.length) return;

    var base = document.body.dataset.basePath || '';
    var connEl = document.getElementById('live-conn');
    var feed = {};   // build id -> newest record from the feed
    var sock = null, everOpen = false, tries = 0, retryTimer = null, pollTimer = null;

    // ---- row markup (mirrors index.html / project.html; keep in step) ----
    function el(tag, cls, text) {
      var e = document.createElement(tag);
      if (cls) e.className = cls;
      if (text !== undefined) e.textContent = text;
      return e;
    }
    function buildLink(b, cls, text) {
      var a = el('a', cls, text);
      a.href = base + '/builds/' + b.id;
      return a;
    }
    function timeCell(b) {
      var t = el('time');
      t.setAttribute('data-abs', '');
      t.setAttribute('datetime', b.started_at || b.created_at || '');
      setAbsLabel(t);
      return t;
    }

    function buildCard(b) {
      var card = el('div', 'card');
      var header = el('div', 'card-header');
      var left = el('div');
      left.appendChild(buildLink(b, 'card-title', (b.project_name || 'build') + ' #' + b.id));
      left.appendChild(el('div', 'meta', b.commit_message || ''));
      header.appendChild(left);
      header.appendChild(el('span', 'badge badge-' + b.status, b.status));
      var when = el('div', 'meta');
      when.appendChild(timeCell(b));
      card.appendChild(header);
      card.appendChild(when);
      card.appendChild(el('div', 'meta build-dur'));
      return card;
    }

    function buildRow(b) {
      var tr = el('tr');
      var idCell = el('td');
      idCell.appendChild(buildLink(b, null, String(b.id)));
      tr.appendChild(idCell);
      tr.appendChild(el('td', 'meta', b.commit_sha || ''));
      tr.appendChild(el('td', null, b.commit_message || ''));
      var statusCell = el('td');
      statusCell.appendChild(el('span', 'badge badge-' + b.status, b.status));
      tr.appendChild(statusCell);
      var whenCell = el('td', 'meta');
      whenCell.appendChild(timeCell(b));
      tr.appendChild(whenCell);
      tr.appendChild(el('td', 'meta build-dur'));
      var logCell = el('td');
      logCell.appendChild(buildLink(b, 'btn', 'Logs'));
      tr.appendChild(logCell);
      return tr;
    }

    // ---- rendering ----
    function setBadge(row, status) {
      var b = row.querySelector('.badge');
      if (b) {
        b.className = 'badge badge-' + status;
        b.textContent = status;
      }
      row.dataset.status = status;
    }

    // Tiny inline progress bar for a running row; indeterminate without an
    // estimate or on a real overrun (same rules as the build page).
    function miniProgress(elapsedSec, expectedSec) {
      var wrap = el('span', 'mini-progress');
      var fill = el('span', 'mini-progress-fill');
      var pct = expectedSec > 0 ? (elapsedSec / expectedSec) * 100 : -1;
      if (pct >= 0 && pct <= 100) {
        fill.style.width = pct.toFixed(1) + '%';
      } else if (pct > 100 && pct <= 115) {
        fill.style.width = '98%';
      } else {
        wrap.className += ' mini-progress--indeterminate';
      }
      wrap.appendChild(fill);
      return wrap;
    }

    function renderDuration(row, b) {
      var d = row.querySelector('.build-dur');
      if (!d) return;
      if (b.status === 'running') {
        var elapsed = b.started_at ? (Date.now() - new Date(b.started_at).getTime()) / 1000 : 0;
        var txt = (b.current_step ? (STEP_NAMES[b.current_step] || b.current_step) + ' · ' : '') + fmtShort(elapsed);
        if (b.expected_secs) txt += ' / ~' + fmtShort(b.expected_secs);
        d.textContent = '';
        d.appendChild(miniProgress(elapsed, b.expected_secs));
        d.appendChild(document.createTextNode(txt));
      } else if (b.status === 'pending') {
        d.textContent = b.queue_position >= 2 ? 'queued · #' + b.queue_position : 'queued';
      } else {
        d.textContent = (b.started_at && b.finished_at)
          ? fmtShort((new Date(b.finished_at) - new Date(b.started_at)) / 1000)
          : '';
      }
    }

    // A queued build has no start time until it is claimed; swap the column
    // over to the real one when it arrives.
    function renderWhen(row, b) {
      var t = row.querySelector('time[data-abs]');
      if (!t || !b.started_at) return;
      if (t.getAttribute('datetime') === b.started_at) return;
      t.setAttribute('datetime', b.started_at);
      setAbsLabel(t);
    }

    // ---- list reconciliation ----
    function rowFor(list, id) {
      return list.querySelector('[data-live-build="' + id + '"]');
    }
    function limitOf(list) {
      return parseInt(list.dataset.liveLimit, 10) || 0;
    }
    function rowsOf(list) {
      return list.querySelectorAll('[data-live-build]');
    }
    // A list only accepts builds it would have rendered itself: its project
    // (when scoped) and inside its own window of newest rows.
    function accepts(list, b) {
      var only = list.dataset.liveProject;
      if (only && String(b.project_id) !== only) return false;
      var rows = rowsOf(list), limit = limitOf(list);
      if (!limit || rows.length < limit) return true;
      return b.id > parseInt(rows[rows.length - 1].dataset.liveBuild, 10);
    }
    function insertOrdered(list, node, id) {
      var rows = rowsOf(list);
      for (var i = 0; i < rows.length; i++) {
        if (parseInt(rows[i].dataset.liveBuild, 10) < id) {
          list.insertBefore(node, rows[i]);
          return;
        }
      }
      list.appendChild(node);
    }
    function trim(list) {
      var limit = limitOf(list);
      if (!limit) return;
      var rows = rowsOf(list);
      for (var i = limit; i < rows.length; i++) rows[i].parentNode.removeChild(rows[i]);
    }

    function upsert(list, b) {
      var row = rowFor(list, b.id);
      if (!row) {
        if (!accepts(list, b)) return;
        row = list.dataset.liveKind === 'row' ? buildRow(b) : buildCard(b);
        row.dataset.liveBuild = b.id;
        insertOrdered(list, row, b.id);
        var empty = list.querySelector('[data-live-empty]');
        if (empty) empty.parentNode.removeChild(empty);
        trim(list);
      }
      if (row.dataset.status !== b.status) setBadge(row, b.status);
      renderWhen(row, b);
      renderDuration(row, b);
    }

    function apply(msg) {
      if (!msg || msg.type !== 'builds' || !msg.builds) return;
      // Replace rather than merge: a snapshot is the whole truth, and this
      // keeps the local clock's work bounded on a page left open for days.
      feed = {};
      msg.builds.forEach(function (b) {
        feed[b.id] = b;
        lists.forEach(function (list) { upsert(list, b); });
      });
    }

    // Local clock: keeps elapsed time and the progress bar moving between
    // pushes, so an idle-but-running dashboard costs nothing on the wire.
    setInterval(function () {
      Object.keys(feed).forEach(function (id) {
        var b = feed[id];
        if (b.status !== 'running') return;
        lists.forEach(function (list) {
          var row = rowFor(list, id);
          if (row) renderDuration(row, b);
        });
      });
    }, 1000);

    // ---- transport ----
    function setConn(kind) {
      if (!connEl) return;
      if (!kind) { connEl.hidden = true; return; }
      connEl.hidden = false;
      connEl.className = 'log-conn' + (kind === 'live' ? ' log-conn--live' : kind === 'polling' ? ' log-conn--polling' : '');
      connEl.textContent = kind === 'live' ? '● live' : kind === 'polling' ? '◌ polling' : '↻ reconnecting';
    }

    function connect() {
      if (!window.WebSocket) { startPolling(); return; }
      var url = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + base + '/api/live';
      var s;
      try { s = new WebSocket(url); } catch (e) { startPolling(); return; }
      sock = s;
      s.onopen = function () {
        everOpen = true;
        tries = 0;
        setConn('live');
        stopPolling();
      };
      s.onmessage = function (e) {
        try { apply(JSON.parse(e.data)); } catch (err) { /* ignore a bad frame */ }
      };
      s.onclose = function () {
        if (sock !== s) return; // superseded by a newer socket
        sock = null;
        setConn(pollTimer ? 'polling' : 'reconnecting');
        scheduleReconnect();
      };
    }

    function scheduleReconnect() {
      tries++;
      // Two failures without ever connecting means the socket is not getting
      // through — an old proxy that won't forward Upgrade, most likely. Start
      // polling, but keep retrying in the background so the page heals itself
      // once that is fixed.
      if (!everOpen && tries >= 2) startPolling();
      clearTimeout(retryTimer);
      retryTimer = setTimeout(connect, Math.min(30000, 1000 * Math.pow(2, Math.min(tries, 5))));
    }

    function startPolling() {
      if (pollTimer) return;
      setConn('polling');
      var tick = function () {
        fetch(base + '/api/live')
          .then(function (r) { return r.json(); })
          .then(apply)
          .catch(function () {})
          .then(function () {
            if (pollTimer) pollTimer = setTimeout(tick, 4000);
          });
      };
      pollTimer = setTimeout(tick, 0);
    }

    function stopPolling() {
      if (!pollTimer) return;
      clearTimeout(pollTimer);
      pollTimer = null;
    }

    connect();
  }

  // --- Project page: trigger button ---
  function initTriggerButton() {
    var triggerBtn = document.getElementById('trigger-build');
    if (!triggerBtn) return;
    triggerBtn.addEventListener('click', function () {
      triggerBtn.disabled = true;
      triggerBtn.textContent = 'Triggering...';
      fetch(triggerBtn.dataset.url, { method: 'POST', headers: { 'X-Builds-Csrf': '1' } })
        .then(function (r) { return r.json(); })
        .then(function () { location.reload(); })
        .catch(function () {
          triggerBtn.disabled = false;
          triggerBtn.textContent = 'Build';
        });
    });
  }

  // --- Build page ---
  function initBuildPage() {
    var root = document.getElementById('build-log');
    if (!root) return;
    var ds = root.dataset;

    var body = document.getElementById('log-body');
    var emptyState = document.getElementById('log-empty');
    var truncEl = document.getElementById('log-trunc');
    var chip = document.getElementById('status-chip');
    var btnCancel = document.getElementById('btn-cancel');
    var btnRerun = document.getElementById('btn-rerun');
    var shaChip = document.getElementById('sha-chip');
    var stepsNav = document.getElementById('steps');
    var callout = document.getElementById('failure-callout');
    var connEl = document.getElementById('log-conn');
    var logCount = document.getElementById('log-count');
    var searchBox = document.getElementById('log-search');
    var searchCount = document.getElementById('search-count');
    var tglFollow = document.getElementById('tgl-follow');
    var tglWrap = document.getElementById('tgl-wrap');
    var btnCopy = document.getElementById('btn-copy');
    var followPill = document.getElementById('follow-pill');
    var metaDuration = document.getElementById('meta-duration');
    var metaStarted = document.getElementById('meta-started');
    var progressBar = document.getElementById('build-progress');
    var progressFill = document.getElementById('build-progress-fill');
    var queuePosEl = document.getElementById('queue-pos');
    var queuePosN = document.getElementById('queue-pos-n');

    var MAX_DOM_LINES = 20000;
    var MAX_MEM_LINES = 60000;
    var FLUSH_MIN_MS = 100;

    var STEP_LABELS = { clone: 'Clone', checkout: 'Checkout', build: 'Build', push: 'Push', deploy: 'Deploy' };
    var STEP_RE = /^\[(\d{2}:\d{2}:\d{2})\] ##\[step:([a-z]+)\] (.*)$/;
    var MARKER_RE = /^\[\d{2}:\d{2}:\d{2}\] /;
    var OK_RE = /^\[\d{2}:\d{2}:\d{2}\] BUILD SUCCESS/;
    var ERROR_RE = /^\[ERROR\] /;

    var state = {
      status: ds.status,
      startedAt: ds.startedAt ? new Date(ds.startedAt) : null,
      finishedAt: ds.finishedAt ? new Date(ds.finishedAt) : null,
      offset: parseInt(ds.logLen, 10) || 0, // server-side byte offset
      carry: '',
      lines: [],       // {text: display text (CR-resolved, ANSI-stripped), cls}
      firstLine: 0,    // absolute index of lines[0]
      total: 0,        // total lines ever seen
      domCount: 0,
      domFirst: 0,
      pendingEls: [],
      tailEl: null,
      ansiClasses: [],
      steps: [],       // {id, label, clock, abs}
      lastError: -1,
      matches: [],
      painted: [],
      cur: -1,
      follow: false,
      progScroll: false,
      es: null, esErrors: 0, esGotEvent: false,
      poller: null, metaPoller: null, watchdog: null,
      ticker: null, expectedSecs: 0,
      rafScheduled: false, lastFlush: 0, flushTimer: null,
      lastFavColor: null,
    };

    var baseTitle = (ds.projectName || 'build') + ' #' + ds.buildId;

    function isTerminal(s) { return s === 'success' || s === 'failed' || s === 'canceled'; }
    function pad(n) { return (n < 10 ? '0' : '') + n; }

    // ---- ANSI handling ----
    var CSI_OTHER_RE = /\x1b\[[0-9;?]*[@-ln-~]/g;   // CSI with any final byte except 'm'
    var OSC_RE = /\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?/g;
    var ESC_SINGLE_RE = /\x1b[^\[\]]/g;
    var SGR_RE = /\x1b\[([0-9;]*)m/g;

    function stripOtherEscapes(s) {
      if (s.indexOf('\x1b') === -1) return s;
      return s.replace(OSC_RE, '').replace(CSI_OTHER_RE, '').replace(ESC_SINGLE_RE, '');
    }
    function stripAllAnsi(s) {
      if (s.indexOf('\x1b') === -1) return s;
      return stripOtherEscapes(s).replace(SGR_RE, '');
    }
    function applySGR(params) {
      var parts = (params || '0').split(';');
      for (var i = 0; i < parts.length; i++) {
        var n = parseInt(parts[i] || '0', 10);
        if (n === 0) { state.ansiClasses = []; }
        else if (n === 1) { addCls('a-b'); }
        else if (n === 22) { rmCls('a-b'); }
        else if (n === 39) { rmColor(); }
        else if ((n >= 30 && n <= 37) || (n >= 90 && n <= 97)) { rmColor(); addCls('a-' + n); }
        else if (n === 38 || n === 48) { // consume extended color params, ignore
          var mode = parseInt(parts[i + 1], 10);
          i += (mode === 5) ? 2 : (mode === 2 ? 4 : 0);
        }
      }
      function addCls(c) { if (state.ansiClasses.indexOf(c) === -1) state.ansiClasses.push(c); }
      function rmCls(c) { var j = state.ansiClasses.indexOf(c); if (j !== -1) state.ansiClasses.splice(j, 1); }
      function rmColor() {
        state.ansiClasses = state.ansiClasses.filter(function (c) { return c === 'a-b'; });
      }
    }
    // Tokenize one line into [{t, c[]}] spans; SGR state persists across lines.
    function ansiSpans(line) {
      line = stripOtherEscapes(line);
      if (line.indexOf('\x1b') === -1) {
        return [{ t: line, c: state.ansiClasses.slice() }];
      }
      var spans = [], last = 0, m;
      SGR_RE.lastIndex = 0;
      while ((m = SGR_RE.exec(line))) {
        if (m.index > last) spans.push({ t: line.slice(last, m.index), c: state.ansiClasses.slice() });
        applySGR(m[1]);
        last = SGR_RE.lastIndex;
      }
      if (last < line.length) spans.push({ t: line.slice(last), c: state.ansiClasses.slice() });
      return spans;
    }

    // ---- Render pipeline ----
    function crResolve(line) {
      if (line.indexOf('\r') === -1) return line;
      var segs = line.split('\r');
      for (var i = segs.length - 1; i >= 0; i--) {
        if (segs[i] !== '') return segs[i];
      }
      return '';
    }

    function addLine(raw) {
      var resolved = crResolve(raw);
      var stripped = stripAllAnsi(resolved);
      var cls = '', display = resolved, m;
      if ((m = STEP_RE.exec(stripped))) {
        cls = 'log-line--step';
        display = '[' + m[1] + '] ' + m[3];
        state.steps.push({ id: m[2], label: STEP_LABELS[m[2]] || m[2], clock: m[1], abs: state.total });
        stripped = display;
      } else if (ERROR_RE.test(stripped)) {
        cls = 'log-line--err';
        state.lastError = state.total;
      } else if (OK_RE.test(stripped)) {
        cls = 'log-line--ok';
        display = stripped; // marker rendering slices by column; strip ANSI
      } else if (MARKER_RE.test(stripped)) {
        cls = 'log-line--marker';
        display = stripped;
      }

      var abs = state.total++;
      state.lines.push({ text: stripped, cls: cls });
      if (state.lines.length > MAX_MEM_LINES) {
        state.lines.splice(0, state.lines.length - MAX_MEM_LINES);
        state.firstLine = state.total - state.lines.length;
      }
      state.pendingEls.push(buildLineEl(abs, display, cls));
    }

    function buildLineEl(abs, display, cls) {
      var div = document.createElement('div');
      div.className = cls ? 'log-line ' + cls : 'log-line';
      div.id = 'L' + (abs + 1);
      var a = document.createElement('a');
      a.className = 'log-ln';
      a.href = '#L' + (abs + 1);
      a.textContent = String(abs + 1);
      var span = document.createElement('span');
      span.className = 'log-txt';
      if (cls === 'log-line--step' || cls === 'log-line--ok' || cls === 'log-line--marker') {
        // Runner marker lines: timestamp muted, no ANSI inside.
        var ts = document.createElement('span');
        ts.className = 'log-ts';
        ts.textContent = display.slice(0, 10);
        span.appendChild(ts);
        span.appendChild(document.createTextNode(stripAllAnsi(display).slice(10)));
      } else {
        var spans = ansiSpans(display);
        for (var i = 0; i < spans.length; i++) {
          if (!spans[i].t) continue;
          if (spans[i].c.length) {
            var s = document.createElement('span');
            s.className = spans[i].c.join(' ');
            s.textContent = spans[i].t;
            span.appendChild(s);
          } else {
            span.appendChild(document.createTextNode(spans[i].t));
          }
        }
      }
      div.appendChild(a);
      div.appendChild(span);
      return div;
    }

    function processChunk(text) {
      if (!text) return;
      if (emptyState && !emptyState.hidden) emptyState.hidden = true;
      text = state.carry + text;
      // Hold back a partial trailing escape sequence.
      var holdEsc = '';
      var pe = /\x1b(\[[0-9;?]*)?$/.exec(text);
      if (pe) { holdEsc = text.slice(pe.index); text = text.slice(0, pe.index); }
      var parts = text.split('\n');
      state.carry = parts.pop() + holdEsc;
      for (var i = 0; i < parts.length; i++) addLine(parts[i]);
      // A pathological never-terminated line would grow carry (and its
      // re-render cost) without bound — promote it to a real line.
      if (state.carry.length > 65536) {
        addLine(state.carry);
        state.carry = '';
      }
      scheduleFlush();
    }

    function drainCarry() {
      if (state.carry) {
        addLine(state.carry);
        state.carry = '';
      }
    }

    function scheduleFlush() {
      if (state.rafScheduled) return;
      var since = Date.now() - state.lastFlush;
      if (since < FLUSH_MIN_MS) {
        if (!state.flushTimer) {
          state.flushTimer = setTimeout(function () {
            state.flushTimer = null;
            scheduleFlush();
          }, FLUSH_MIN_MS - since);
        }
        return;
      }
      state.rafScheduled = true;
      // rAF doesn't fire in background tabs — pendingEls would grow without
      // bound there; fall back to a timer so hidden tabs keep draining.
      if (document.hidden) {
        setTimeout(flush, 250);
      } else {
        requestAnimationFrame(flush);
      }
    }

    function flush() {
      state.rafScheduled = false;
      state.lastFlush = Date.now();
      var appended = state.pendingEls.length;
      if (appended) {
        var frag = document.createDocumentFragment();
        for (var i = 0; i < appended; i++) frag.appendChild(state.pendingEls[i]);
        state.pendingEls = [];
        if (state.tailEl && state.tailEl.parentNode === body) {
          body.insertBefore(frag, state.tailEl);
        } else {
          body.appendChild(frag);
        }
        state.domCount += appended;
        trimDom();
        renderSteps();
      }
      renderTail();
      updateCounts();
      if (state.follow) {
        scrollToBottom();
      } else if (appended && state.status === 'running') {
        followPill.hidden = false;
      }
      if (appended && searchBox.value.trim()) runSearch(false);
    }

    function trimDom() {
      var excess = state.domCount - MAX_DOM_LINES;
      if (excess <= 0) return;
      for (var i = 0; i < excess; i++) {
        var first = body.firstChild;
        if (!first || first === state.tailEl) break;
        body.removeChild(first);
        state.domCount--;
        state.domFirst++;
      }
      showTrunc();
    }

    function showTrunc() {
      truncEl.hidden = false;
      truncEl.textContent = 'Showing the last ' + (state.total - state.domFirst).toLocaleString() +
        ' of ' + state.total.toLocaleString() + ' lines — ';
      var a = document.createElement('a');
      a.href = ds.logUrl + '?download=1';
      a.textContent = 'Download full log';
      truncEl.appendChild(a);
    }

    function renderTail() {
      var t = crResolve(stripAllAnsi(state.carry));
      if (!t) {
        if (state.tailEl && state.tailEl.parentNode) state.tailEl.parentNode.removeChild(state.tailEl);
        return;
      }
      if (!state.tailEl) {
        state.tailEl = document.createElement('div');
        state.tailEl.className = 'log-line';
        var ln = document.createElement('span');
        ln.className = 'log-ln';
        var tx = document.createElement('span');
        tx.className = 'log-txt';
        state.tailEl.appendChild(ln);
        state.tailEl.appendChild(tx);
      }
      state.tailEl.firstChild.textContent = String(state.total + 1);
      state.tailEl.lastChild.textContent = t;
      body.appendChild(state.tailEl); // keep at the end
    }

    function updateCounts() {
      logCount.textContent = state.total ? state.total.toLocaleString() + ' lines' : '';
    }

    // ---- Steps rail ----
    var ICONS = {
      check: '<svg width="16" height="16" viewBox="0 0 16 16" fill="none"><circle cx="8" cy="8" r="6.5" stroke="currentColor" stroke-width="1.5"/><path d="M5 8.2l2 2L11 6" stroke="currentColor" stroke-width="1.5" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>',
      x: '<svg width="16" height="16" viewBox="0 0 16 16" fill="none"><circle cx="8" cy="8" r="6.5" stroke="currentColor" stroke-width="1.5"/><path d="M5.8 5.8l4.4 4.4M10.2 5.8l-4.4 4.4" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>',
      skip: '<svg width="16" height="16" viewBox="0 0 16 16" fill="none"><circle cx="8" cy="8" r="6.5" stroke="currentColor" stroke-width="1.5"/><path d="M4.5 11.5l7-7" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"/></svg>',
    };

    function clockToSec(clock) {
      var p = clock.split(':');
      return (+p[0]) * 3600 + (+p[1]) * 60 + (+p[2]);
    }
    function utcClockSec(d) {
      return d.getUTCHours() * 3600 + d.getUTCMinutes() * 60 + d.getUTCSeconds();
    }
    function stepDurSec(i) {
      var st = state.steps[i];
      var start = clockToSec(st.clock);
      var end = null;
      if (i + 1 < state.steps.length) end = clockToSec(state.steps[i + 1].clock);
      else if (state.finishedAt) end = utcClockSec(state.finishedAt);
      else if (state.status === 'running') end = utcClockSec(new Date());
      if (end === null) return null;
      var d = end - start;
      if (d < 0) d += 86400; // midnight wrap
      // A running step compares the CLIENT clock against server timestamps;
      // a few seconds of skew would render as ~24h via the wrap correction.
      if (d > 43200) d = 0;
      return d;
    }
    function fmtDur(sec) {
      if (sec === null || isNaN(sec)) return '';
      if (sec < 60) return sec + 's';
      var m = Math.floor(sec / 60), s = sec % 60;
      if (m < 60) return m + 'm ' + pad(s) + 's';
      return Math.floor(m / 60) + 'h ' + pad(m % 60) + 'm';
    }

    function renderSteps() {
      if (!state.steps.length) { stepsNav.hidden = true; return; }
      stepsNav.hidden = false;
      stepsNav.textContent = '';
      var succeeded = state.status === 'success';
      state.steps.forEach(function (st, i) {
        var last = i === state.steps.length - 1;
        var btn = document.createElement('button');
        btn.type = 'button';
        var kind = 'done', iconHTML = ICONS.check, iconNode = null;
        if (last && !succeeded) {
          if (state.status === 'running' || state.status === 'pending') {
            kind = 'running';
            iconNode = document.createElement('span');
            iconNode.className = 'spinner';
          } else if (state.status === 'failed') {
            kind = 'failed'; iconHTML = ICONS.x;
          } else if (state.status === 'canceled') {
            kind = 'canceled'; iconHTML = ICONS.skip;
          }
        }
        btn.className = 'step step--' + kind;
        var icon = document.createElement('span');
        icon.className = 'step-icon';
        if (iconNode) icon.appendChild(iconNode);
        else icon.innerHTML = iconHTML; // static SVG constants only, never log data
        btn.appendChild(icon);
        btn.appendChild(document.createTextNode(st.label));
        var dur = fmtDur(stepDurSec(i));
        if (dur) {
          var ds2 = document.createElement('span');
          ds2.className = 'step-dur';
          ds2.textContent = dur;
          btn.appendChild(ds2);
        }
        btn.addEventListener('click', function () { jumpToLine(st.abs); });
        stepsNav.appendChild(btn);
      });
    }

    function jumpToLine(abs, center) {
      var el = document.getElementById('L' + (abs + 1));
      if (!el) return;
      setFollow(false);
      el.scrollIntoView({ block: center ? 'center' : 'start' });
      el.classList.remove('log-line--flash');
      void el.offsetWidth; // restart the animation
      el.classList.add('log-line--flash');
    }

    // ---- Failure callout ----
    function renderCallout() {
      callout.textContent = '';
      if (state.status !== 'failed' && state.status !== 'canceled') return;
      var div = document.createElement('div');
      div.className = 'callout ' + (state.status === 'failed' ? 'callout--error' : 'callout--neutral');
      var title = document.createElement('span');
      title.className = 'callout-title';
      if (state.status === 'canceled') {
        title.textContent = 'Build canceled';
        div.appendChild(title);
      } else {
        var stepName = state.steps.length ? state.steps[state.steps.length - 1].label : '';
        var durTxt = (state.startedAt && state.finishedAt) ? fmtElapsed(state.finishedAt - state.startedAt) : '';
        title.textContent = 'Build failed' + (stepName ? ' in ' + stepName : '') + (durTxt ? ' · ' + durTxt : '');
        div.appendChild(title);
        if (state.lastError >= state.firstLine) {
          var rec = state.lines[state.lastError - state.firstLine];
          if (rec) {
            var msg = document.createElement('span');
            msg.className = 'callout-msg';
            msg.textContent = rec.text.replace(/^\[ERROR\] /, '');
            div.appendChild(msg);
          }
          var jump = document.createElement('button');
          jump.type = 'button';
          jump.className = 'callout-jump';
          jump.textContent = 'Jump to error ↓';
          jump.addEventListener('click', function () { jumpToLine(state.lastError, true); });
          div.appendChild(jump);
        }
      }
      callout.appendChild(div);
    }

    // ---- Status, duration, title ----
    function fmtElapsed(ms) {
      var s = Math.max(0, Math.floor(ms / 1000));
      var h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
      return h ? h + ':' + pad(m) + ':' + pad(sec) : m + ':' + pad(sec);
    }

    function updateDuration() {
      if (state.startedAt && state.finishedAt) {
        metaDuration.textContent = fmtElapsed(state.finishedAt - state.startedAt);
      } else if (state.startedAt && state.status === 'running') {
        var elapsedMs = Date.now() - state.startedAt.getTime();
        var txt = fmtElapsed(elapsedMs);
        if (state.expectedSecs > 0) {
          if (elapsedMs / 1000 <= state.expectedSecs * 1.15) {
            txt += ' / ~' + fmtElapsed(state.expectedSecs * 1000);
          } else {
            txt += ' · usually ' + fmtElapsed(state.expectedSecs * 1000);
          }
          metaDuration.title = 'Estimate from the last successful builds';
        }
        metaDuration.textContent = txt;
      } else {
        metaDuration.textContent = '—';
      }
    }

    // Progress bar: fill toward the estimate while running. Honesty rules —
    // hold at 98% on a slight overrun, switch to an indeterminate sweep when
    // there is no estimate or the build is genuinely overrunning.
    function updateProgress() {
      if (!progressBar) return;
      if (state.status !== 'running' || !state.startedAt) {
        progressBar.hidden = true;
        return;
      }
      progressBar.hidden = false;
      var elapsed = (Date.now() - state.startedAt.getTime()) / 1000;
      var pct = state.expectedSecs > 0 ? (elapsed / state.expectedSecs) * 100 : -1;
      if (pct >= 0 && pct <= 100) {
        progressBar.classList.remove('build-progress--indeterminate');
        progressFill.style.width = pct.toFixed(1) + '%';
        progressBar.setAttribute('aria-valuenow', String(Math.round(pct)));
      } else if (pct > 100 && pct <= 115) {
        progressBar.classList.remove('build-progress--indeterminate');
        progressFill.style.width = '98%';
        progressBar.setAttribute('aria-valuenow', '98');
      } else {
        progressBar.classList.add('build-progress--indeterminate');
        progressFill.style.width = '';
        progressBar.removeAttribute('aria-valuenow');
      }
    }

    // Pull the expected duration (mean of recent successful builds) once
    // per running phase; the ticker then renders elapsed vs expected.
    function fetchExpected() {
      if (state.status !== 'running') return;
      fetch(ds.apiUrl + '?meta=1')
        .then(function (r) { return r.json(); })
        .then(function (meta) {
          state.expectedSecs = meta.expected_secs || 0;
          updateDuration();
          updateProgress();
        })
        .catch(function () {});
    }

    function manageTicker() {
      var want = state.status === 'running' && state.startedAt;
      if (want && !state.ticker) {
        state.ticker = setInterval(function () {
          updateDuration();
          updateProgress();
          updateTitleFavicon();
          if (state.steps.length) renderSteps();
        }, 1000);
      } else if (!want && state.ticker) {
        clearInterval(state.ticker);
        state.ticker = null;
      }
    }

    function updateTitleFavicon() {
      var prefix = '', color = '#8b949e';
      switch (state.status) {
        case 'running':
          prefix = '▶ ' + (state.startedAt ? fmtElapsed(Date.now() - state.startedAt.getTime()) + ' · ' : '');
          color = '#d2991d';
          break;
        case 'pending': prefix = '… '; break;
        case 'success': prefix = '✓ '; color = '#3fb950'; break;
        case 'failed': prefix = '✗ '; color = '#f85149'; break;
        case 'canceled': prefix = '⊘ '; break;
      }
      document.title = prefix + baseTitle + ' — Build-System';
      if (color !== state.lastFavColor) {
        state.lastFavColor = color;
        var c = document.createElement('canvas');
        c.width = c.height = 32;
        var g = c.getContext('2d');
        g.beginPath();
        g.arc(16, 16, 10, 0, Math.PI * 2);
        g.fillStyle = color;
        g.fill();
        var fav = document.getElementById('favicon');
        if (fav) fav.href = c.toDataURL('image/png');
      }
    }

    function applyStatus(d) {
      if (!d || !d.status) return;
      var prev = state.status;
      state.status = d.status;
      if (d.started_at) state.startedAt = new Date(d.started_at);
      if (d.finished_at) state.finishedAt = new Date(d.finished_at);
      root.dataset.status = d.status;

      chip.className = 'status-chip status-chip--' + d.status;
      chip.textContent = '';
      var icon = document.createElement('span');
      icon.className = 'status-chip-icon';
      chip.appendChild(icon);
      chip.appendChild(document.createTextNode(d.status));

      var active = d.status === 'pending' || d.status === 'running';
      btnCancel.hidden = !active;
      btnRerun.hidden = active;
      if (!active) { btnCancel.disabled = false; btnCancel.textContent = 'Cancel'; }
      tglFollow.hidden = !active && d.status !== 'running';

      if (d.status !== 'pending') {
        if (emptyState) emptyState.hidden = true;
        stopMetaPoll();
      }
      if (d.status === 'running' && prev === 'pending') {
        tglFollow.hidden = false;
        setFollow(true); // watch the log as soon as the queued build starts
        fetchExpected();
      }
      if (state.startedAt && metaStarted) {
        metaStarted.setAttribute('datetime', state.startedAt.toISOString());
      }
      updateDuration();
      updateProgress();
      manageTicker();
      updateTitleFavicon();
      renderRelTimes();

      if (isTerminal(d.status)) {
        drainCarry();
        flush();
        renderSteps();
        renderCallout();
        stopTransport();
        followPill.hidden = true;
        tglFollow.hidden = true;
      }
    }

    // ---- Transport: SSE with polling fallback ----
    function setConn(kind) {
      if (!kind) { connEl.hidden = true; return; }
      connEl.hidden = false;
      connEl.className = 'log-conn' + (kind === 'live' ? ' log-conn--live' : kind === 'polling' ? ' log-conn--polling' : '');
      connEl.textContent = kind === 'live' ? '● live' : kind === 'polling' ? '◌ polling' : '↻ reconnecting';
    }

    function resetWatchdog() {
      clearTimeout(state.watchdog);
      if (isTerminal(state.status)) return;
      state.watchdog = setTimeout(function () {
        // Stream stalled (buffering proxy?) — fall back to polling.
        if (state.es) { state.es.close(); state.es = null; }
        startPolling();
      }, 45000);
    }

    function connectSSE() {
      if (!window.EventSource) { startPolling(); return; }
      var es = new EventSource(ds.eventsUrl + '?offset=' + state.offset);
      state.es = es;
      setConn('live');
      var alive = function () {
        state.esGotEvent = true;
        state.esErrors = 0;
        setConn('live');
        resetWatchdog();
      };
      es.addEventListener('log', function (e) {
        alive();
        try {
          var d = JSON.parse(e.data);
          processChunk(d.t);
          state.offset = d.o;
        } catch (err) { /* malformed frame; resume via reconnect */ }
      });
      es.addEventListener('status', function (e) {
        alive();
        try { applyStatus(JSON.parse(e.data)); } catch (err) {}
      });
      es.addEventListener('ping', alive);
      es.onerror = function () {
        if (isTerminal(state.status)) { es.close(); state.es = null; return; }
        if (!state.esGotEvent) {
          state.esErrors++;
          if (state.esErrors >= 2) {
            es.close();
            state.es = null;
            startPolling();
            return;
          }
        }
        setConn('reconnecting'); // EventSource auto-reconnects with Last-Event-ID
      };
      resetWatchdog();
    }

    function startPolling() {
      if (state.poller || isTerminal(state.status)) return;
      setConn('polling');
      clearTimeout(state.watchdog);
      state.poller = true;
      // Chained timeout, not setInterval: a slow response must finish (and
      // advance state.offset) before the next request is issued, otherwise
      // overlapping fetches replay the same offset and duplicate log lines.
      var tick = function () {
        if (!state.poller) return;
        fetch(ds.logUrl + '?offset=' + state.offset)
          .then(function (res) {
            if (!res.ok) return null;
            var total = parseInt(res.headers.get('X-Log-Offset'), 10);
            var st = res.headers.get('X-Build-Status');
            return res.text().then(function (text) {
              if (text) processChunk(text);
              if (!isNaN(total) && total > state.offset) state.offset = total;
              if (st && st !== state.status && st !== 'pending') {
                // Pull timestamps once, then apply.
                return fetch(ds.apiUrl + '?meta=1')
                  .then(function (r) { return r.json(); })
                  .then(function (meta) {
                    applyStatus({ status: st, started_at: meta.started_at, finished_at: meta.finished_at });
                  })
                  .catch(function () { applyStatus({ status: st }); });
              }
            });
          })
          .catch(function () {})
          .then(function () {
            if (state.poller) state.pollTimer = setTimeout(tick, 1500);
          });
      };
      tick();
    }

    function stopTransport() {
      if (state.es) { state.es.close(); state.es = null; }
      state.poller = false;
      clearTimeout(state.pollTimer);
      clearTimeout(state.watchdog);
      setConn(null);
    }

    // ---- Queue position (pending only) ----
    function startMetaPoll() {
      if (state.metaPoller || state.status !== 'pending') return;
      state.metaPoller = setInterval(function () {
        fetch(ds.apiUrl + '?meta=1')
          .then(function (r) { return r.json(); })
          .then(function (meta) {
            if (meta.status === 'pending') {
              var show = meta.queue_position >= 2;
              if (queuePosEl) queuePosEl.hidden = !show;
              if (queuePosN && show) queuePosN.textContent = meta.queue_position;
            } else {
              applyStatus({ status: meta.status, started_at: meta.started_at, finished_at: meta.finished_at });
            }
          })
          .catch(function () {});
      }, 4000);
    }
    function stopMetaPoll() {
      if (state.metaPoller) { clearInterval(state.metaPoller); state.metaPoller = null; }
    }

    // ---- Follow mode ----
    function scrollToBottom() {
      var before = body.scrollTop;
      state.progScroll = true;
      body.scrollTop = body.scrollHeight;
      if (body.scrollTop === before) {
        // No-op scroll fires no event; a stale flag would misattribute the
        // user's next scroll as programmatic.
        state.progScroll = false;
      }
    }
    function setFollow(on) {
      state.follow = on;
      tglFollow.classList.toggle('on', on);
      if (on) {
        followPill.hidden = true;
        scrollToBottom();
      }
    }
    body.addEventListener('scroll', function () {
      if (state.progScroll) { state.progScroll = false; return; }
      var dist = body.scrollHeight - body.scrollTop - body.clientHeight;
      if (dist > 40) {
        if (state.follow) {
          state.follow = false;
          tglFollow.classList.remove('on');
        }
      } else if (dist < 4 && !state.follow && state.status === 'running') {
        setFollow(true);
      }
    });
    tglFollow.addEventListener('click', function () { setFollow(!state.follow); });
    followPill.addEventListener('click', function () { setFollow(true); });

    // ---- Wrap / copy ----
    tglWrap.addEventListener('click', function () {
      var on = !tglWrap.classList.contains('on');
      tglWrap.classList.toggle('on', on);
      root.classList.toggle('log-viewer--nowrap', !on);
    });

    function copyText(t) {
      if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(t);
      return new Promise(function (resolve, reject) {
        var ta = document.createElement('textarea');
        ta.value = t;
        ta.style.position = 'fixed';
        ta.style.opacity = '0';
        document.body.appendChild(ta);
        ta.select();
        try { document.execCommand('copy') ? resolve() : reject(); }
        catch (e) { reject(e); }
        finally { document.body.removeChild(ta); }
      });
    }
    btnCopy.addEventListener('click', function () {
      var text = state.lines.map(function (l) { return l.text; }).join('\n');
      if (state.firstLine > 0) {
        text = '… earlier lines omitted; full log: ' + location.origin + ds.logUrl + '?download=1\n' + text;
      }
      copyText(text).then(function () {
        btnCopy.textContent = 'Copied';
        setTimeout(function () { btnCopy.textContent = 'Copy'; }, 1200);
      }).catch(function () {});
    });

    if (shaChip) {
      shaChip.addEventListener('click', function () {
        copyText(shaChip.dataset.sha).then(function () {
          var orig = shaChip.textContent;
          shaChip.textContent = 'copied';
          shaChip.classList.add('sha-chip--copied');
          setTimeout(function () {
            shaChip.textContent = orig;
            shaChip.classList.remove('sha-chip--copied');
          }, 1200);
        }).catch(function () {});
      });
    }

    // ---- Search ----
    var searchTimer = null;
    searchBox.addEventListener('input', function () {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(function () { runSearch(true); }, 150);
    });
    searchBox.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') {
        e.preventDefault();
        stepMatch(e.shiftKey ? -1 : 1);
      } else if (e.key === 'Escape') {
        searchBox.value = '';
        runSearch(false);
        searchBox.blur();
      }
    });

    function clearPainted() {
      for (var i = 0; i < state.painted.length; i++) {
        var el = document.getElementById('L' + (state.painted[i] + 1));
        if (el) el.classList.remove('log-line--match', 'log-line--cur');
      }
      state.painted = [];
    }
    function runSearch(scroll) {
      // Keep the user's place when re-running for streamed lines.
      var keepAbs = (state.cur >= 0 && state.matches[state.cur] !== undefined) ? state.matches[state.cur] : -1;
      clearPainted();
      state.matches = [];
      state.cur = -1;
      var q = searchBox.value.trim().toLowerCase();
      if (!q) { searchCount.textContent = ''; return; }
      for (var i = 0; i < state.lines.length; i++) {
        if (state.lines[i].text.toLowerCase().indexOf(q) !== -1) {
          state.matches.push(state.firstLine + i);
        }
      }
      for (var j = 0; j < state.matches.length; j++) {
        var el = document.getElementById('L' + (state.matches[j] + 1));
        if (el) { el.classList.add('log-line--match'); state.painted.push(state.matches[j]); }
      }
      if (state.matches.length) {
        state.cur = 0;
        if (keepAbs >= 0) {
          var prevIdx = state.matches.indexOf(keepAbs);
          if (prevIdx !== -1) {
            state.cur = prevIdx;
            scroll = false; // same match, don't yank the view
          }
        }
        paintCur(scroll);
      }
      searchCount.textContent = state.matches.length ? (state.cur + 1) + '/' + state.matches.length : '0/0';
    }
    function paintCur(scroll) {
      for (var i = 0; i < state.painted.length; i++) {
        var el = document.getElementById('L' + (state.painted[i] + 1));
        if (el) el.classList.remove('log-line--cur');
      }
      if (state.cur < 0 || !state.matches.length) return;
      var abs = state.matches[state.cur];
      var cur = document.getElementById('L' + (abs + 1));
      if (cur) {
        cur.classList.add('log-line--cur');
        if (scroll) {
          setFollow(false);
          cur.scrollIntoView({ block: 'center' });
        }
      }
      searchCount.textContent = (state.cur + 1) + '/' + state.matches.length;
    }
    function stepMatch(dir) {
      if (!state.matches.length) return;
      state.cur = (state.cur + dir + state.matches.length) % state.matches.length;
      paintCur(true);
    }

    // ---- Cancel / re-run ----
    btnCancel.addEventListener('click', function () {
      if (!confirm('Cancel this build?')) return;
      btnCancel.disabled = true;
      btnCancel.textContent = 'Canceling…';
      fetch(ds.cancelUrl, { method: 'POST', headers: { 'X-Builds-Csrf': '1' } })
        .then(function (res) {
          if (res.status !== 200 && res.status !== 202) throw new Error('cancel failed');
          // Status flips via SSE/polling; safety net if no event arrives.
          setTimeout(function () {
            if (!isTerminal(state.status)) {
              fetch(ds.apiUrl + '?meta=1')
                .then(function (r) { return r.json(); })
                .then(function (meta) {
                  if (isTerminal(meta.status)) {
                    applyStatus({ status: meta.status, started_at: meta.started_at, finished_at: meta.finished_at });
                  }
                })
                .catch(function () {});
            }
          }, 5000);
        })
        .catch(function () {
          btnCancel.disabled = false;
          btnCancel.textContent = 'Cancel';
        });
    });

    btnRerun.addEventListener('click', function () {
      btnRerun.disabled = true;
      fetch(ds.rerunUrl, { method: 'POST', headers: { 'X-Builds-Csrf': '1' } })
        .then(function (res) {
          if (res.status !== 201) throw new Error('rerun failed');
          return res.json();
        })
        .then(function (nb) { location.href = ds.buildPageBase + nb.id; })
        .catch(function () { btnRerun.disabled = false; });
    });

    // ---- Relative times ----
    var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
    function renderRelTimes() {
      var els = document.querySelectorAll('[data-rel]');
      for (var i = 0; i < els.length; i++) {
        var dt = els[i].getAttribute('datetime');
        if (!dt) { els[i].textContent = '—'; continue; }
        var d = new Date(dt);
        els[i].title = d.toLocaleString();
        els[i].textContent = relTime(d);
      }
    }
    function relTime(d) {
      var s = Math.floor((Date.now() - d.getTime()) / 1000);
      if (s < 45) return 'just now';
      if (s < 3600) return Math.max(1, Math.round(s / 60)) + 'm ago';
      if (s < 86400) return Math.round(s / 3600) + 'h ago';
      return MONTHS[d.getMonth()] + ' ' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
    }
    setInterval(renderRelTimes, 30000);

    // ---- Initial paint ----
    var initialText = body.textContent;
    body.textContent = '';
    if (initialText.trim() || initialText.length > 0) {
      var lines = initialText.split('\n');
      var lastPartial = lines.pop(); // '' when the log ends with \n
      if (lines.length > MAX_DOM_LINES) {
        var skipped = lines.length - MAX_DOM_LINES;
        state.total = skipped;
        state.firstLine = skipped;
        state.domFirst = skipped;
        lines = lines.slice(skipped);
      }
      for (var li = 0; li < lines.length; li++) addLine(lines[li]);
      state.carry = lastPartial;
      if (isTerminal(state.status)) drainCarry();
      if (state.domFirst > 0) showTrunc();
    }
    flush();
    renderSteps();
    renderCallout();
    updateDuration();
    updateProgress();
    manageTicker();
    updateTitleFavicon();
    renderRelTimes();
    updateCounts();

    tglFollow.hidden = !(state.status === 'running' || state.status === 'pending');
    if (state.status === 'running') setFollow(true);

    if (!isTerminal(state.status)) {
      connectSSE();
      startMetaPoll();
      fetchExpected();
    }

    // #L123 anchor
    var am = /^#L(\d+)$/.exec(location.hash);
    if (am) {
      var target = parseInt(am[1], 10) - 1;
      setTimeout(function () { jumpToLine(target, true); }, 50);
    }
  }
})();
