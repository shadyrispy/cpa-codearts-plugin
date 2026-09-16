package main

import "strings"

// panelHTML returns the browser dashboard served at
// /v0/resource/plugins/codearts-provider/panel.
//
// Resource routes are NOT admin-authenticated, so this page holds no secrets of
// its own: it reads the admin key from the management UI's localStorage (as
// documented for same-origin admin deployments) and calls the authenticated
// /v0/management/... routes. All credential-bearing endpoints are behind that
// key. Scripts are inlined rather than loaded from a CDN so the admin page never
// pulls third-party code into an admin context.
func panelHTML() string {
	return strings.ReplaceAll(panelTemplate, "__PROVIDER__", providerID)
}

const panelTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CodeArts</title>
<style>
  :root {
    --bg: #0f1115; --card: #171a21; --card2: #1e222b; --line: #2a2f3a;
    --fg: #e6e8ee; --dim: #9aa3b2; --accent: #4c8dff; --ok: #35c28a;
    --warn: #e8b339; --err: #e8604c;
  }
  @media (prefers-color-scheme: light) {
    :root { --bg:#f5f6f8; --card:#fff; --card2:#f0f2f5; --line:#dfe3ea;
            --fg:#1c1f26; --dim:#5d6675; }
  }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--fg);
         font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif; }
  .wrap { max-width:1100px; margin:0 auto; padding:24px 20px 60px; }
  header { display:flex; align-items:center; gap:12px; flex-wrap:wrap; margin-bottom:6px; }
  h1 { font-size:19px; margin:0; font-weight:600; }
  .badge { font-size:12px; padding:2px 8px; border-radius:999px;
           background:var(--card2); color:var(--dim); border:1px solid var(--line); }
  .sub { color:var(--dim); font-size:13px; margin-bottom:18px; }
  .bar { display:flex; gap:8px; flex-wrap:wrap; margin-bottom:18px; }
  button { font:inherit; padding:7px 13px; border-radius:8px; cursor:pointer;
           background:var(--card2); color:var(--fg); border:1px solid var(--line); }
  button:hover { border-color:var(--accent); }
  button.primary { background:var(--accent); border-color:var(--accent); color:#fff; }
  button:disabled { opacity:.55; cursor:not-allowed; }
  button.danger:hover { border-color:var(--err); color:var(--err); }
  .card { background:var(--card); border:1px solid var(--line); border-radius:12px;
          padding:16px 18px; margin-bottom:16px; }
  .card h2 { font-size:14px; margin:0 0 12px; font-weight:600; color:var(--dim);
             text-transform:uppercase; letter-spacing:.04em; }
  .grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(250px,1fr)); gap:12px; }
  .acct { background:var(--card2); border:1px solid var(--line); border-radius:10px; padding:13px 14px; }
  .acct .top { display:flex; justify-content:space-between; gap:8px; align-items:baseline; }
  .acct .who { font-weight:600; word-break:break-all; }
  .acct .meta { color:var(--dim); font-size:12px; margin-top:3px; word-break:break-all; }
  .pill { font-size:11px; padding:2px 7px; border-radius:999px; border:1px solid var(--line); color:var(--dim); white-space:nowrap; }
  .pill.on { color:var(--ok); border-color:var(--ok); }
  .pill.off { color:var(--err); border-color:var(--err); }
  .meter { margin-top:10px; }
  .meter .lbl { display:flex; justify-content:space-between; font-size:12px; color:var(--dim); }
  .track { height:6px; border-radius:999px; background:var(--line); overflow:hidden; margin-top:4px; }
  .fill { height:100%; background:var(--accent); border-radius:999px; }
  .fill.hi { background:var(--warn); }
  .fill.max { background:var(--err); }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th,td { text-align:left; padding:7px 8px; border-bottom:1px solid var(--line); vertical-align:top; }
  th { color:var(--dim); font-weight:500; font-size:12px; text-transform:uppercase; letter-spacing:.03em; }
  td.mono,.mono { font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; font-size:12px; }
  .err { color:var(--err); }
  .ok { color:var(--ok); }
  .empty { color:var(--dim); padding:8px 0; }
  #msg { min-height:20px; font-size:13px; margin-bottom:12px; }
  details { margin-top:10px; }
  summary { cursor:pointer; color:var(--dim); font-size:13px; }
  pre { background:var(--card2); border:1px solid var(--line); border-radius:8px;
        padding:10px; overflow:auto; font-size:12px; max-height:320px; }
  input,textarea { font:inherit; background:var(--card2); color:var(--fg);
        border:1px solid var(--line); border-radius:8px; padding:7px 9px; width:100%; }
  textarea { font-family:ui-monospace,Menlo,Consolas,monospace; font-size:12px; min-height:110px; resize:vertical; }
  .row { display:flex; gap:8px; flex-wrap:wrap; align-items:flex-end; }
  .row > div { flex:1 1 190px; }
  label { display:block; font-size:12px; color:var(--dim); margin-bottom:3px; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>CodeArts</h1>
    <span class="badge">__PROVIDER__</span>
    <span class="badge" id="ver">…</span>
    <span class="badge" id="mode">…</span>
  </header>
  <div class="sub">Subscription, quota and scheduled tasks for Huawei CodeArts through CLIProxyAPI.</div>

  <div id="msg"></div>

  <div class="card">
    <h2>Connection and sign-in</h2>
    <label for="managementKey">CPA management key (kept in this page's memory)</label>
    <input id="managementKey" type="password" autocomplete="off" placeholder="Enter your CPA management key">
    <div class="bar"><button id="connect">Connect</button><button id="login">Sign in to Huawei Cloud</button></div>
    <a id="loginURL" target="_blank" rel="noopener noreferrer" hidden>Open Huawei sign-in</a>
    <div id="loginStatus" role="status"></div>
    <details><summary>CPA is on a remote server</summary>
      <p>After signing in, paste the localhost callback URL here if your browser cannot open it.</p>
      <input id="callbackURL" type="password" autocomplete="off" placeholder="http://127.0.0.1:…/authentication?secret=…">
      <button id="submitCallback">Complete remote sign-in</button>
    </details>
  </div>

  <div class="bar">
    <button class="primary" id="refresh">Refresh</button>
    <button id="refreshQuota">Refresh quota</button>
    <button id="runAll">Run scheduled tasks now</button>
  </div>

  <div class="card">
    <h2>Accounts</h2>
    <div id="accounts" class="grid"><div class="empty">Loading…</div></div>
  </div>

  <div class="card">
    <h2>Scheduled tasks</h2>
    <div id="schedule"><div class="empty">Loading…</div></div>
  </div>

  <div class="card">
    <h2>Import credential</h2>
    <div class="row">
      <div><label>Access key ID</label><input id="ak" placeholder="AK..."></div>
      <div><label>Secret access key</label><input id="sk" placeholder="SK..." type="password"></div>
      <div><label>Security token (optional)</label><input id="tok" placeholder="STS token"></div>
    </div>
    <div class="row" style="margin-top:8px">
      <div><label>Domain ID (optional)</label><input id="dom" placeholder="domain id"></div>
      <div><label>User name (optional)</label><input id="usr" placeholder="you"></div>
      <div><label>File name (optional)</label><input id="fname" placeholder="__PROVIDER__-me.json"></div>
    </div>
    <div style="margin-top:10px"><button class="primary" id="doImport">Import</button></div>
    <details>
      <summary>Or paste a credential JSON</summary>
      <div style="margin-top:8px">
        <textarea id="json" placeholder='{"access_key_id":"...","secret_access_key":"...","security_token":"...","domain_id":"..."}'></textarea>
        <div style="margin-top:8px"><button id="doImportJson">Import JSON</button></div>
      </div>
    </details>
  </div>

  <div class="card">
    <h2>Advanced</h2>
    <div class="bar" style="margin:0">
      <button id="showStatus">Show status JSON</button>
      <button id="showExport">Show export JSON</button>
    </div>
    <pre id="raw" hidden></pre>
  </div>
</div>

<script>
(function () {
  "use strict";
  var P = "__PROVIDER__";
  var BASE = "/v0/management/" + P;

  // The management UI keeps the admin key in same-origin localStorage. Resource
  // pages are not authenticated themselves, so privileged calls carry this key.
  function adminKey() {
	var entered = document.getElementById("managementKey").value.trim();
	if (entered) return entered;
    var keys = ["apiKey", "api_key", "adminKey", "cliproxy_api_key", "cpa_api_key"];
    for (var i = 0; i < keys.length; i++) {
      var v = window.localStorage.getItem(keys[i]);
      if (v) return v;
    }
    return "";
  }

  function headers() {
    var h = { "Content-Type": "application/json" };
    var k = adminKey();
    if (k) { h["Authorization"] = "Bearer " + k; h["X-API-Key"] = k; }
    return h;
  }

  function say(text, kind) {
    var el = document.getElementById("msg");
    el.textContent = text || "";
    el.className = kind || "";
  }

  function call(path, opts) {
    opts = opts || {};
    return fetch(path, {
      method: opts.method || "GET",
      headers: headers(),
      body: opts.body ? JSON.stringify(opts.body) : undefined
    }).then(function (r) {
      return r.text().then(function (t) {
        var data = null;
        try { data = t ? JSON.parse(t) : null; } catch (e) { data = { raw: t }; }
        if (!r.ok) {
          var m = (data && (data.error || data.message)) || ("HTTP " + r.status);
          throw new Error(m);
        }
        return data;
      });
    });
  }

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function meter(label, pct) {
    if (pct === null || pct === undefined || pct < 0) return "";
    var v = Number(pct);
    if (!isFinite(v)) return "";
    var cls = v >= 90 ? "max" : (v >= 70 ? "hi" : "");
    return '<div class="meter"><div class="lbl"><span>' + esc(label) + '</span><span>' +
      v.toFixed(1) + '% used</span></div><div class="track"><div class="fill ' + cls +
      '" style="width:' + Math.max(0, Math.min(100, v)) + '%"></div></div></div>';
  }

  function renderAccounts(list) {
    var host = document.getElementById("accounts");
    if (!list || !list.length) {
      host.innerHTML = '<div class="empty">No ' + esc(P) + ' accounts yet. Sign in or import a credential below.</div>';
      return;
    }
    host.innerHTML = list.map(function (a) {
      var pill = a.disabled ? '<span class="pill off">disabled</span>'
                            : '<span class="pill on">active</span>';
      var head = esc(a.user_name || a.label || a.name || a.auth_index);
      var bits = [];
      if (a.plan || a.plan_name) bits.push(esc(a.plan_name || a.plan));
      if (a.login_type) bits.push(esc(a.login_type));
      if (a.expires_at) bits.push("expires " + esc(a.expires_at));
      if (a.reset_date) bits.push("resets " + esc(a.reset_date));
      return '<div class="acct"><div class="top"><span class="who">' + head + '</span>' + pill + '</div>' +
        (bits.length ? '<div class="meta">' + bits.join(" · ") + '</div>' : '') +
        meter("Code completions", a.code_completions_percent) +
        meter("Chat messages", a.chat_messages_percent) +
        (a.quota_error ? '<div class="meta err">' + esc(a.quota_error) + '</div>' : '') +
        '<div class="meta mono">' + esc(a.auth_index) + '</div>' +
        '<div style="margin-top:9px"><button class="danger" data-del="' + esc(a.auth_index) + '">Delete</button></div>' +
        '</div>';
    }).join("");
  }

  function renderSchedule(s) {
    var host = document.getElementById("schedule");
    var tasks = (s && s.tasks) || [];
    if (!tasks.length) {
      host.innerHTML = '<div class="empty">The scheduler is off and no tasks are configured.</div>';
      return;
    }
    var rows = tasks.map(function (t) {
      var state = t.last_error ? '<span class="err">' + esc(t.last_error) + '</span>'
                  : (t.last_result ? '<span class="ok">' + esc(t.last_result) + '</span>' : "—");
      return '<tr><td class="mono">' + esc(t.id) + '</td><td class="mono">' + esc(t.type) + '</td>' +
        '<td class="mono">' + esc(t.cron) + '</td>' +
        '<td>' + (t.enabled === false ? '<span class="pill off">off</span>' : '<span class="pill on">on</span>') + '</td>' +
        '<td class="mono">' + esc(t.next_run || "—") + '</td>' +
        '<td class="mono">' + esc(t.last_run || "—") + '</td>' +
        '<td>' + state + '</td>' +
        '<td><button data-run="' + esc(t.id) + '">Run</button></td></tr>';
    }).join("");
    host.innerHTML = '<div class="sub" style="margin-bottom:8px">' +
      (s.enabled ? '<span class="ok">scheduler enabled</span>' : '<span class="pill off">scheduler disabled</span>') +
      ' · timezone ' + esc(s.timezone || "local") + '</div>' +
      '<table><thead><tr><th>Task</th><th>Type</th><th>Cron</th><th>State</th>' +
      '<th>Next run</th><th>Last run</th><th>Result</th><th></th></tr></thead><tbody>' +
      rows + '</tbody></table>';
  }

  function load() {
    say("Loading…");
    Promise.all([
      call(BASE + "/accounts"),
      call(BASE + "/schedule")
    ]).then(function (out) {
      renderAccounts(out[0] && out[0].accounts);
      renderSchedule(out[1]);
      say("");
    }).catch(function (e) {
      say("Load failed: " + e.message + " (the management API needs the admin key; open this page from the CLIProxyAPI management UI)", "err");
    });
  }

  function loadMeta() {
    // Public status resource: no admin key needed.
    fetch("/v0/resource/plugins/" + P + "/status").then(function (r) { return r.json(); })
      .then(function (d) {
        var p = (d && d.plugin) || {};
        document.getElementById("ver").textContent = "v" + (p.version || "?");
        var e = (d && d.endpoint) || {};
        document.getElementById("mode").textContent = (e.protocol_mode || "?") + " · " + (e.base_url || "");
      }).catch(function () {});
  }

  document.getElementById("refresh").onclick = load;
  document.getElementById("connect").onclick = load;
  var loginState = "", loginTimer = null, loginDeadline = 0;
  function pollLogin() {
    if (!loginState || Date.now() > loginDeadline) {
      document.getElementById("loginStatus").textContent = "Sign-in expired. Start again.";
      return;
    }
    call("/v0/management/get-auth-status?state=" + encodeURIComponent(loginState)).then(function(r) {
      if (r.status === "ok") {
        document.getElementById("loginStatus").textContent = "Account saved in CPA.";
        document.getElementById("callbackURL").value = "";
        loginState = ""; load(); return;
      }
      if (r.status === "error") throw new Error(r.error || r.message || "Sign-in failed");
      loginTimer = setTimeout(pollLogin, 2000);
    }).catch(function(e) { document.getElementById("loginStatus").textContent = e.message; });
  }
  document.getElementById("login").onclick = function() {
    clearTimeout(loginTimer);
    call("/v0/management/" + P + "-auth-url").then(function(r) {
      loginState = r.state; loginDeadline = Date.now() + 300000;
      var link = document.getElementById("loginURL");
      link.href = r.url; link.hidden = false;
      document.getElementById("loginStatus").textContent = "Open the sign-in link and authorize your account.";
      pollLogin();
    }).catch(function(e) { say(e.message, "err"); });
  };
  document.getElementById("submitCallback").onclick = function() {
    call(BASE + "/login/callback", {method:"POST", body:{state:loginState, callback_url:document.getElementById("callbackURL").value.trim()}})
      .then(function() { document.getElementById("callbackURL").value = ""; say("Callback received; waiting for CPA to save the account."); })
      .catch(function(e) { say(e.message, "err"); });
  };
  document.getElementById("refreshQuota").onclick = function () {
    say("Refreshing quota…");
    call(BASE + "/quota/refresh", { method: "POST", body: {} })
      .then(function (r) {
        say("Quota refreshed for " + (r.refreshed || 0) + " account(s)" +
            (r.failed ? ", " + r.failed + " failed" : ""), r.failed ? "err" : "");
        setTimeout(load, 400);
      }).catch(function (e) { say("Quota refresh failed: " + e.message, "err"); });
  };

  document.getElementById("runAll").onclick = function () {
    var btns = document.querySelectorAll("[data-run]");
    if (!btns.length) { say("No tasks to run.", "err"); return; }
    var ids = [];
    for (var i = 0; i < btns.length; i++) ids.push(btns[i].getAttribute("data-run"));
    say("Starting " + ids.length + " task(s)…");
    Promise.all(ids.map(function (id) {
      return call(BASE + "/schedule/run", { method: "POST", body: { task: id } }).catch(function () { return null; });
    })).then(function () {
      say("Tasks started; refreshing state…");
      setTimeout(load, 1200);
    });
  };

  document.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t || t.tagName !== "BUTTON") return;
    var del = t.getAttribute("data-del");
    var run = t.getAttribute("data-run");
    if (del) {
      if (!window.confirm("Delete this account and its auth file?\n\n" + del)) return;
      t.disabled = true;
      call(BASE + "/delete", { method: "POST", body: { auth_index: del } })
        .then(function () { say("Account deleted."); load(); })
        .catch(function (e) { say("Delete failed: " + e.message, "err"); t.disabled = false; });
    } else if (run) {
      t.disabled = true;
      call(BASE + "/schedule/run", { method: "POST", body: { task: run } })
        .then(function () { say("Task " + run + " started."); setTimeout(load, 1200); })
        .catch(function (e) { say("Run failed: " + e.message, "err"); t.disabled = false; });
    }
  });

  function doImport(payload) {
    say("Importing…");
    call(BASE + "/import", { method: "POST", body: payload })
      .then(function (r) {
        say("Imported as " + (r.name || "?") + ".", "");
        load();
      }).catch(function (e) { say("Import failed: " + e.message, "err"); });
  }

  document.getElementById("doImport").onclick = function () {
    doImport({
      access_key_id: document.getElementById("ak").value.trim(),
      secret_access_key: document.getElementById("sk").value.trim(),
      security_token: document.getElementById("tok").value.trim(),
      domain_id: document.getElementById("dom").value.trim(),
      user_name: document.getElementById("usr").value.trim(),
      name: document.getElementById("fname").value.trim()
    });
  };

  document.getElementById("doImportJson").onclick = function () {
    var raw = document.getElementById("json").value.trim();
    if (!raw) { say("Paste a credential JSON first.", "err"); return; }
    var parsed;
    try { parsed = JSON.parse(raw); } catch (e) { say("That is not valid JSON.", "err"); return; }
    doImport(parsed);
  };

  document.getElementById("showStatus").onclick = function () {
    fetch("/v0/resource/plugins/" + P + "/status").then(function (r) { return r.text(); })
      .then(function (t) {
        var pre = document.getElementById("raw");
        pre.hidden = false; pre.textContent = t;
      });
  };

  document.getElementById("showExport").onclick = function () {
    say("Fetching export…");
    call(BASE + "/export").then(function (d) {
      var pre = document.getElementById("raw");
      pre.hidden = false; pre.textContent = JSON.stringify(d, null, 2);
      say("Export contains live credentials — handle it carefully.", "err");
    }).catch(function (e) { say("Export failed: " + e.message, "err"); });
  };

  loadMeta();
  load();
})();
</script>
</body>
</html>
`
