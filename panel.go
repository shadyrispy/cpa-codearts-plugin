package main

import "strings"

// panelHTML returns the browser dashboard served at
// /v0/resource/plugins/codearts-provider/panel.
//
// The page follows the VS Code extension's own model: there is exactly one way
// to sign in, the browser authorization the extension performs ("登录" ->
// authorize in the CodeArts console -> the account appears). Everything else on
// the page reports state (accounts, quota, scheduled tasks), it is not another
// way to log in. The credential-import routes still exist for operators who
// already hold an AK/SK pair, but they are not part of the authorization flow
// and are documented in README.md instead of being a second button here.
//
// Resource routes are NOT admin-authenticated, so this page holds no secrets of
// its own: it reads the admin key from the management UI's localStorage (as
// documented for same-origin admin deployments) and calls the authenticated
// /v0/management/... routes. Scripts are inlined rather than loaded from a CDN so
// the admin page never pulls third-party code into an admin context.
func panelHTML() string {
	return strings.ReplaceAll(panelTemplate, "__PROVIDER__", providerID)
}

const panelTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CodeArts 订阅</title>
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
         font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif; }
  .wrap { max-width:1100px; margin:0 auto; padding:24px 20px 60px; }
  header { display:flex; align-items:center; gap:12px; flex-wrap:wrap; margin-bottom:6px; }
  h1 { font-size:19px; margin:0; font-weight:600; }
  .badge { font-size:12px; padding:2px 8px; border-radius:999px;
           background:var(--card2); color:var(--dim); border:1px solid var(--line); }
  .sub { color:var(--dim); font-size:13px; margin-bottom:18px; }
  .bar { display:flex; gap:8px; flex-wrap:wrap; margin-bottom:14px; }
  button { font:inherit; padding:7px 13px; border-radius:8px; cursor:pointer;
           background:var(--card2); color:var(--fg); border:1px solid var(--line); }
  button:hover { border-color:var(--accent); }
  button.primary { background:var(--accent); border-color:var(--accent); color:#fff; }
  button:disabled { opacity:.55; cursor:not-allowed; }
  button.danger:hover { border-color:var(--err); color:var(--err); }
  .card { background:var(--card); border:1px solid var(--line); border-radius:12px;
          padding:16px 18px; margin-bottom:16px; }
  .card h2 { font-size:14px; margin:0 0 12px; font-weight:600; color:var(--dim); }
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
  th { color:var(--dim); font-weight:500; font-size:12px; }
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
  textarea { font-family:ui-monospace,Menlo,Consolas,monospace; font-size:12px; min-height:80px; resize:vertical; }
  .row { display:flex; gap:8px; flex-wrap:wrap; align-items:flex-end; }
  .row > div { flex:1 1 190px; }
  label { display:block; font-size:12px; color:var(--dim); margin-bottom:3px; }
  .steps { margin:0; padding-left:20px; color:var(--dim); font-size:13px; }
  .steps li { margin:3px 0; }
  .status { border:1px dashed var(--line); border-radius:8px; padding:9px 11px;
            font-size:13px; margin-top:10px; }
</style>
</head>
<body>
<div class="wrap">
  <header>
    <h1>CodeArts 订阅</h1>
    <span class="badge">__PROVIDER__</span>
    <span class="badge" id="ver">…</span>
    <span class="badge" id="mode">…</span>
  </header>
  <div class="sub">把华为 CodeArts Doer（CodeArts Agent / 编程助手）账号授权给 CLIProxyAPI，供 Claude Code、OpenAI 兼容客户端等调用。</div>

  <div id="msg"></div>

  <div class="card">
    <h2>授权</h2>
    <label for="managementKey">CPA 管理密钥（仅保存在当前页面内存中，刷新后需重新输入）</label>
    <input id="managementKey" type="password" autocomplete="off" placeholder="输入 CPA 管理密钥">
    <div class="bar" style="margin-top:10px">
      <button class="primary" id="login">开始授权</button>
      <button id="refresh">刷新状态</button>
    </div>
    <ol class="steps">
      <li>点击「开始授权」，下方会出现登录链接。</li>
      <li>打开链接，在 CodeArts 控制台完成登录授权。</li>
      <li>本机 CPA 会自动接收回调；远程 CPA 请复制浏览器最终的 localhost 地址并在下方提交。</li>
    </ol>
    <div class="status" id="loginStatus">尚未开始。请先填写管理密钥。</div>
    <div id="loginLinkBox" hidden style="margin-top:8px">
      <a id="loginURL" target="_blank" rel="noopener noreferrer">点此打开华为授权页面</a>
    </div>

    <details>
      <summary>授权页面打不开 127.0.0.1（CPA 在容器 / 远程服务器上）</summary>
      <p>OAuth 完成后会跳到一个无法打开的 <span class="mono">http://127.0.0.1:端口/oauth/callback?code=...&amp;state=...</span> 页面。这是远程部署的预期行为：把地址栏里的完整地址复制下来，粘贴到下面，CPA 会用其中的一次性 code 换取并保存凭证。</p>
      <textarea id="callbackURL" autocomplete="off" placeholder="http://127.0.0.1:40000/oauth/callback?code=...&state=..."></textarea>
      <div style="margin-top:8px"><button id="submitCallback">提交回调地址，完成授权</button></div>
      <p class="sub" style="margin:8px 0 0">authorization code 为一次性凭证，请立即提交且不要分享；无需把回调端口映射到公网。</p>
    </details>
  </div>

  <div class="card">
    <h2>账号</h2>
    <div id="accounts" class="grid"><div class="empty">加载中…</div></div>
  </div>

  <div class="card">
    <h2>定时任务</h2>
    <div class="bar">
      <button id="runAll">立即执行全部任务</button>
      <button id="refreshQuota">刷新额度</button>
    </div>
    <div id="schedule"><div class="empty">加载中…</div></div>
  </div>

  <div class="card">
    <h2>诊断</h2>
    <div class="bar" style="margin:0">
      <button id="showStatus">查看运行状态</button>
      <button id="showExport">查看凭证导出（含密钥）</button>
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
    return '<div class="meter"><div class="lbl"><span>' + esc(label) + '</span><span>已用 ' +
      v.toFixed(1) + '%</span></div><div class="track"><div class="fill ' + cls +
      '" style="width:' + Math.max(0, Math.min(100, v)) + '%"></div></div></div>';
  }

  function renderAccounts(list) {
    var host = document.getElementById("accounts");
    if (!list || !list.length) {
      host.innerHTML = '<div class="empty">还没有账号。点击上方「开始授权」完成登录。</div>';
      return;
    }
    host.innerHTML = list.map(function (a) {
      var pill = a.disabled ? '<span class="pill off">已停用</span>'
                            : '<span class="pill on">可用</span>';
      var head = esc(a.user_name || a.label || a.name || a.auth_index);
      var bits = [];
      if (a.plan || a.plan_name) bits.push("套餐：" + esc(a.plan_name || a.plan));
      if (a.login_type) bits.push("登录方式：" + esc(a.login_type));
      if (a.expires_at) bits.push("凭证到期：" + esc(a.expires_at));
      if (a.reset_date) bits.push("额度重置：" + esc(a.reset_date));
      return '<div class="acct"><div class="top"><span class="who">' + head + '</span>' + pill + '</div>' +
        (bits.length ? '<div class="meta">' + bits.join(" · ") + '</div>' : '') +
        meter("代码补全额度", a.code_completions_percent) +
        meter("对话消息额度", a.chat_messages_percent) +
        (a.quota_error ? '<div class="meta err">额度查询失败：' + esc(a.quota_error) + '</div>' : '') +
        '<div class="meta mono">' + esc(a.auth_index) + '</div>' +
        '<div style="margin-top:9px"><button class="danger" data-del="' + esc(a.auth_index) + '">删除账号</button></div>' +
        '</div>';
    }).join("");
  }

  function renderSchedule(s) {
    var host = document.getElementById("schedule");
    var tasks = (s && s.tasks) || [];
    if (!tasks.length) {
      host.innerHTML = '<div class="empty">调度未启用，也没有配置任务。</div>';
      return;
    }
    var rows = tasks.map(function (t) {
      var state = t.last_error ? '<span class="err">' + esc(t.last_error) + '</span>'
                  : (t.last_result ? '<span class="ok">' + esc(t.last_result) + '</span>' : "—");
      return '<tr><td class="mono">' + esc(t.id) + '</td><td class="mono">' + esc(t.type) + '</td>' +
        '<td class="mono">' + esc(t.cron) + '</td>' +
        '<td>' + (t.enabled === false ? '<span class="pill off">停用</span>' : '<span class="pill on">启用</span>') + '</td>' +
        '<td class="mono">' + esc(t.next_run || "—") + '</td>' +
        '<td class="mono">' + esc(t.last_run || "—") + '</td>' +
        '<td>' + state + '</td>' +
        '<td><button data-run="' + esc(t.id) + '">执行</button></td></tr>';
    }).join("");
    host.innerHTML = '<div class="sub" style="margin-bottom:8px">' +
      (s.enabled ? '<span class="ok">调度已启用</span>' : '<span class="pill off">调度未启用</span>') +
      ' · 时区 ' + esc(s.timezone || "本机时区") + '</div>' +
      '<table><thead><tr><th>任务</th><th>类型</th><th>Cron</th><th>状态</th>' +
      '<th>下次执行</th><th>上次执行</th><th>结果</th><th></th></tr></thead><tbody>' +
      rows + '</tbody></table>';
  }

  function load() {
    say("加载中…");
    Promise.all([
      call(BASE + "/accounts"),
      call(BASE + "/schedule")
    ]).then(function (out) {
      renderAccounts(out[0] && out[0].accounts);
      renderSchedule(out[1]);
      say("");
    }).catch(function (e) {
      say("加载失败：" + e.message + "（管理接口需要 CPA 管理密钥，请填写后重试）", "err");
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

  var loginState = "", loginTimer = null, loginDeadline = 0;
  function setLoginStatus(text, kind) {
    var el = document.getElementById("loginStatus");
    el.textContent = text;
    el.className = "status" + (kind ? " " + kind : "");
  }
  function pollLogin() {
    if (!loginState) return;
    if (Date.now() > loginDeadline) {
      setLoginStatus("授权已超时，请重新点击「开始授权」。", "err");
      loginState = ""; return;
    }
    call("/v0/management/get-auth-status?state=" + encodeURIComponent(loginState)).then(function (r) {
      if (r.status === "ok") {
        setLoginStatus("授权成功，账号已写入 CPA。", "ok");
        document.getElementById("callbackURL").value = "";
        loginState = ""; load(); return;
      }
      if (r.status === "error") {
        setLoginStatus("授权失败：" + (r.error || r.message || "未知原因"), "err");
        loginState = ""; return;
      }
      // "wait" carries no detail, so the plugin's own poll route explains which
      // stage the flow is in (waiting for the browser, or exchanging the code).
      call(BASE + "/login/status?state=" + encodeURIComponent(loginState)).then(function (d) {
        if (d && d.message) setLoginStatus(d.message);
      }).catch(function () {});
      loginTimer = setTimeout(pollLogin, 2000);
    }).catch(function (e) {
      setLoginStatus("查询授权状态失败：" + e.message, "err");
    });
  }
  document.getElementById("login").onclick = function () {
    clearTimeout(loginTimer);
    setLoginStatus("正在创建授权链接…");
    call("/v0/management/" + P + "-auth-url").then(function (r) {
      loginState = r.state; loginDeadline = Date.now() + 300000;
      var link = document.getElementById("loginURL");
      link.href = r.url;
      document.getElementById("loginLinkBox").hidden = false;
      window.open(r.url, "_blank", "noopener");
      setLoginStatus("已打开授权页面。若浏览器没有自动打开，请点击下方链接。");
      pollLogin();
    }).catch(function (e) { setLoginStatus("创建授权链接失败：" + e.message, "err"); });
  };
  document.getElementById("submitCallback").onclick = function () {
    if (!loginState) { say("请先点击「开始授权」。", "err"); return; }
    var raw = document.getElementById("callbackURL").value.trim();
    if (!raw) { say("请粘贴浏览器地址栏中的回调地址。", "err"); return; }
    call(BASE + "/login/callback", { method: "POST", body: { state: loginState, callback_url: raw } })
      .then(function () {
        document.getElementById("callbackURL").value = "";
        setLoginStatus("已收到回调地址，正在换取凭证…");
        if (!loginTimer) pollLogin();
      })
      .catch(function (e) { say("提交回调地址失败：" + e.message, "err"); });
  };

  document.getElementById("refreshQuota").onclick = function () {
    say("正在刷新额度…");
    call(BASE + "/quota/refresh", { method: "POST", body: {} })
      .then(function (r) {
        say("额度已刷新：成功 " + (r.refreshed || 0) + " 个账号" +
            (r.failed ? "，失败 " + r.failed + " 个" : ""), r.failed ? "err" : "");
        setTimeout(load, 400);
      }).catch(function (e) { say("刷新额度失败：" + e.message, "err"); });
  };

  document.getElementById("runAll").onclick = function () {
    var btns = document.querySelectorAll("[data-run]");
    if (!btns.length) { say("没有可执行的任务。", "err"); return; }
    var ids = [];
    for (var i = 0; i < btns.length; i++) ids.push(btns[i].getAttribute("data-run"));
    say("正在执行 " + ids.length + " 个任务…");
    Promise.all(ids.map(function (id) {
      return call(BASE + "/schedule/run", { method: "POST", body: { task: id } }).catch(function () { return null; });
    })).then(function () {
      say("任务已启动，正在刷新状态…");
      setTimeout(load, 1200);
    });
  };

  document.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t || t.tagName !== "BUTTON") return;
    var del = t.getAttribute("data-del");
    var run = t.getAttribute("data-run");
    if (del) {
      if (!window.confirm("确定删除该账号及其凭证文件？\n\n" + del)) return;
      t.disabled = true;
      call(BASE + "/delete", { method: "POST", body: { auth_index: del } })
        .then(function () { say("账号已删除。"); load(); })
        .catch(function (e) { say("删除失败：" + e.message, "err"); t.disabled = false; });
    } else if (run) {
      t.disabled = true;
      call(BASE + "/schedule/run", { method: "POST", body: { task: run } })
        .then(function () { say("任务 " + run + " 已启动。"); setTimeout(load, 1200); })
        .catch(function (e) { say("执行失败：" + e.message, "err"); t.disabled = false; });
    }
  });

  document.getElementById("showStatus").onclick = function () {
    fetch("/v0/resource/plugins/" + P + "/status").then(function (r) { return r.text(); })
      .then(function (t) {
        var pre = document.getElementById("raw");
        pre.hidden = false; pre.textContent = t;
      });
  };

  document.getElementById("showExport").onclick = function () {
    say("正在获取导出内容…");
    call(BASE + "/export").then(function (d) {
      var pre = document.getElementById("raw");
      pre.hidden = false; pre.textContent = JSON.stringify(d, null, 2);
      say("导出内容包含真实密钥，请谨慎处理。", "err");
    }).catch(function (e) { say("导出失败：" + e.message, "err"); });
  };

  loadMeta();
  load();
})();
</script>
</body>
</html>
`
