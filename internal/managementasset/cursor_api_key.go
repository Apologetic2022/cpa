package managementasset

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// cursorAPIKeyManagerScriptID marks the injected widget script so repeated
// injections stay idempotent.
const cursorAPIKeyManagerScriptID = "cpa-cursor-api-key-manager-script"

var (
	// cursorAPIKeyNavAnchorRe locates the OAuth nav entry in the minified panel
	// bundle and captures its icon expression so the new entry can reuse it.
	cursorAPIKeyNavAnchorRe = regexp.MustCompile("\\{path:`/oauth`,labelKey:`nav\\.oauth`,metaKey:`nav_meta\\.oauth`,icon:([^}]*)\\}")

	// cursorAPIKeyRouteAnchorRe locates the OAuth route entry and captures the
	// minified module alias that exposes the jsx factory.
	cursorAPIKeyRouteAnchorRe = regexp.MustCompile("\\{path:`/oauth`,element:\\(0,([A-Za-z_$][A-Za-z0-9_$]*)\\.jsx\\)\\([^)]*\\)\\}")

	// cursorAPIKeyLocaleAnchorRe locates every nav/nav_meta locale table entry
	// for the OAuth page; the Cursor API Key label slots in right after it.
	cursorAPIKeyLocaleAnchorRe = regexp.MustCompile("(oauth:`[^`]*`,)(quota_management:)")
)

// AddCursorAPIKeyManagerToManagementHTML injects the Cursor API key manager
// page (sidebar nav entry, SPA route, locale labels and the widget script)
// into the management control panel HTML. The panel asset is downloaded from
// the upstream Management Center releases and knows nothing about the Cursor
// provider, so the gateway patches it at serve time. The returned HTML equals
// the input when the widget is already present.
func AddCursorAPIKeyManagerToManagementHTML(html string) (string, error) {
	if strings.Contains(html, cursorAPIKeyManagerScriptID) {
		return html, nil
	}

	out, err := insertCursorAPIKeyNav(html)
	if err != nil {
		return html, err
	}
	out, err = insertCursorAPIKeyRoute(out)
	if err != nil {
		return html, err
	}
	out = insertCursorAPIKeyLocales(out)

	idx := strings.LastIndex(out, "</body>")
	if idx < 0 {
		return html, errors.New("management control panel does not support Cursor API key manager injection")
	}
	return out[:idx] + cursorAPIKeyManagerScript + "\n" + out[idx:], nil
}

// insertCursorAPIKeyNav adds the sidebar entry after the OAuth nav item,
// reusing the OAuth icon expression from the minified bundle.
func insertCursorAPIKeyNav(html string) (string, error) {
	if strings.Contains(html, "labelKey:`nav.cursor_api_key`") {
		return html, nil
	}
	m := cursorAPIKeyNavAnchorRe.FindStringSubmatchIndex(html)
	if m == nil {
		return html, errors.New("management html Cursor API Key nav anchor not found")
	}
	icon := html[m[2]:m[3]]
	entry := ",{path:`/cursor-api-key`,labelKey:`nav.cursor_api_key`,metaKey:`nav_meta.cursor_api_key`,icon:" + icon + "}"
	return html[:m[1]] + entry + html[m[1]:], nil
}

// insertCursorAPIKeyRoute registers the SPA route. The route renders an empty
// host div; the widget script watches for it and mounts the native UI, so the
// React router keeps ownership of page lifecycle.
func insertCursorAPIKeyRoute(html string) (string, error) {
	if strings.Contains(html, "path:`/cursor-api-key`,element:") {
		return html, nil
	}
	m := cursorAPIKeyRouteAnchorRe.FindStringSubmatchIndex(html)
	if m == nil {
		return html, errors.New("management html Cursor API Key route anchor not found")
	}
	alias := html[m[2]:m[3]]
	entry := fmt.Sprintf(",{path:`/cursor-api-key`,element:(0,%s.jsx)(\"div\",{id:\"cpa-native-route-host\"})}", alias)
	return html[:m[1]] + entry + html[m[1]:], nil
}

// insertCursorAPIKeyLocales adds the "Cursor API Key" label to every locale's
// nav and nav_meta tables so the sidebar renders a proper title in all
// languages shipped by the upstream panel.
func insertCursorAPIKeyLocales(html string) string {
	if strings.Contains(html, "cursor_api_key:`") {
		return html
	}
	return cursorAPIKeyLocaleAnchorRe.ReplaceAllString(html, "${1}cursor_api_key:`Cursor API Key`,${2}")
}

// cursorAPIKeyManagerScript is the self-contained Cursor API key manager
// widget. It mounts into the #cpa-native-route-host div rendered by the
// injected SPA route, reuses the panel's management key (captured from
// XHR/fetch headers or the panel's local storage) and drives the
// /v0/management/cursor-api-key endpoints. Recovered verbatim from the
// production gateway binary; keep it dependency-free ES5 so it runs inside
// the minified panel bundle without a build step.
const cursorAPIKeyManagerScript = `<script id="cpa-cursor-api-key-manager-script">
(function () {
  'use strict';
  if (window.__cpaCursorAPIKeyManagerLoaded) return;
  window.__cpaCursorAPIKeyManagerLoaded = true;

  var capturedManagementKey = '';
  var managementPath = '/v0/management/';

  function captureAuthorization(value) {
    if (typeof value !== 'string') return;
    var match = value.match(/^\s*Bearer\s+(.+?)\s*$/i);
    if (match && match[1]) capturedManagementKey = match[1];
  }

  var nativeXHROpen = XMLHttpRequest.prototype.open;
  var nativeXHRSetRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
  XMLHttpRequest.prototype.open = function (method, url) {
    this.__cpaCursorManagementRequest = String(url || '').indexOf(managementPath) !== -1;
    return nativeXHROpen.apply(this, arguments);
  };
  XMLHttpRequest.prototype.setRequestHeader = function (name, value) {
    if (this.__cpaCursorManagementRequest && String(name).toLowerCase() === 'authorization') {
      captureAuthorization(String(value || ''));
    }
    return nativeXHRSetRequestHeader.apply(this, arguments);
  };

  var nativeFetch = window.fetch;
  window.fetch = function (input, init) {
    var url = typeof input === 'string' ? input : (input && input.url) || '';
    if (String(url).indexOf(managementPath) !== -1) {
      var headers = new Headers((init && init.headers) || (input && input.headers) || {});
      captureAuthorization(headers.get('authorization') || '');
    }
    return nativeFetch.apply(window, arguments);
  };

  function rememberedManagementKey() {
    try {
      var raw = localStorage.getItem('cli-proxy-auth');
      if (!raw) return '';
      if (raw.indexOf('enc::v1::') === 0) {
        var key = new TextEncoder().encode(
          'cli-proxy-api-webui::secure-storage|' + window.location.host + '|' + navigator.userAgent
        );
        var encoded = atob(raw.slice(9));
        var decoded = new Uint8Array(encoded.length);
        for (var i = 0; i < encoded.length; i += 1) {
          decoded[i] = encoded.charCodeAt(i) ^ key[i % key.length];
        }
        raw = new TextDecoder().decode(decoded);
      }
      var parsed = JSON.parse(raw);
      return String(
        (parsed && parsed.state && parsed.state.managementKey) ||
        (parsed && parsed.managementKey) ||
        ''
      );
    } catch (_) {
      return '';
    }
  }

  function mount(host) {
    if (host.__cpaMounted) return;
    host.__cpaMounted = true;
    try {
    var entries = [];
    var editingIndex = null;
    var status = null;
    var list = null;
    var form = null;
    var formTitle = null;
    var apiKeyInput = null;
    var apiKeyHint = null;
    var managementKeyInput = null;

    function currentManagementKey() {
      return managementKeyInput.value.trim() || capturedManagementKey || rememberedManagementKey();
    }

    function setStatus(message, kind) {
      status.textContent = message;
      status.className = 'cpa-status' + (kind ? ' cpa-status--' + kind : '');
    }

    function escapeHTML(value) {
      return String(value == null ? '' : value)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    }

    async function request(method, path, body) {
      var managementKey = currentManagementKey();
      if (!managementKey) {
        throw new Error('未获取到管理密钥。请先登录管理面板，或在下方的管理密钥框中临时填写。');
      }
      var options = {
        method: method,
        headers: {
          'Authorization': 'Bearer ' + managementKey,
          'Content-Type': 'application/json'
        }
      };
      if (body !== undefined) options.body = JSON.stringify(body);
      var response = await nativeFetch.call(window, path, options);
      var payload = null;
      try {
        payload = await response.json();
      } catch (_) {
        payload = {};
      }
      if (!response.ok) {
        var message = payload && (payload.error || payload.message);
        if (response.status === 401) {
          message = '管理密钥无效或已过期，请重新登录或在下方填写。';
        }
        throw new Error(message || ('请求失败（HTTP ' + response.status + '）'));
      }
      return payload;
    }

    function render() {
      if (!entries.length) {
        list.innerHTML = '<div class="cpa-empty">尚未配置 Cursor API Key</div>';
        return;
      }
      list.innerHTML = entries.map(function (entry) {
        var base = entry['base-url'] || '默认 https://api2.cursor.sh';
        var meta = [
          '<span class="cpa-tag' + (entry.disabled ? ' cpa-tag--off' : ' cpa-tag--ok') + '">' + (entry.disabled ? '已停用' : '启用') + '</span>',
          '<span class="cpa-tag">优先级 ' + (entry.priority || 0) + '</span>',
          '<span class="cpa-tag">' + escapeHTML(base) + '</span>'
        ];
        if (entry.prefix) meta.push('<span class="cpa-tag">前缀 ' + escapeHTML(entry.prefix) + '</span>');
        if (entry['disable-cooling']) meta.push('<span class="cpa-tag">不冷却</span>');
        if (entry['auth-index']) meta.push('<span class="cpa-tag">Auth ' + escapeHTML(entry['auth-index']) + '</span>');
        var success = Number(entry.success || 0);
        var failed = Number(entry.failed || 0);

        var runtime = '<div class="cpa-runtime">' +
          '<span class="cpa-metric">调用 <b>' + (success + failed) + '</b></span>' +
          '<span class="cpa-metric cpa-metric--ok">成功 <b>' + success + '</b></span>' +
          '<span class="cpa-metric cpa-metric--fail">失败 <b>' + failed + '</b></span>' +
          (entry.unavailable ? '<span class="cpa-tag cpa-tag--off">暂不可用</span>' : '') +
          '</div>';

        var lastError = entry.last_error || {};
        var errorMessage = lastError.message || entry.status_message || '';
        var errorHTML = '';
        if (errorMessage) {
          var errorMeta = [];
          if (lastError.code) errorMeta.push('<span>代码 ' + escapeHTML(lastError.code) + '</span>');
          if (lastError.http_status) errorMeta.push('<span>HTTP ' + escapeHTML(lastError.http_status) + '</span>');
          if (lastError.retryable) errorMeta.push('<span>可重试</span>');
          if (entry.next_retry_after) errorMeta.push('<span>下次重试 ' + escapeHTML(new Date(entry.next_retry_after).toLocaleString()) + '</span>');
          if (entry.updated_at) errorMeta.push('<span>状态更新 ' + escapeHTML(new Date(entry.updated_at).toLocaleString()) + '</span>');
          errorHTML = '<div class="cpa-error-box"><div class="cpa-error-title">最近错误</div><div>' + escapeHTML(errorMessage) + '</div>' +
            (errorMeta.length ? '<div class="cpa-error-meta">' + errorMeta.join('') + '</div>' : '') + '</div>';
        }

        return '<section class="card">' +
          '<div class="cpa-card-top"><div class="cpa-key">' + escapeHTML(entry['api-key']) + '</div><div class="cpa-tags">' + meta.join('') + '</div></div>' +
          '<div class="cpa-actions">' +
            '<button type="button" class="btn" data-action="edit" data-index="' + entry.index + '">编辑</button>' +
            '<button type="button" class="btn" data-action="toggle" data-index="' + entry.index + '">' + (entry.disabled ? '启用' : '停用') + '</button>' +
            '<button type="button" class="btn btn-danger" data-action="delete" data-index="' + entry.index + '">删除</button>' +
          '</div>' +
          runtime +
          errorHTML +
        '</section>';
      }).join('');
    }

    async function load() {
      setStatus('正在读取 Cursor API Key…');
      try {
        var payload = await request('GET', '/v0/management/cursor-api-key');
        entries = Array.isArray(payload['cursor-api-key']) ? payload['cursor-api-key'] : [];
        render();
        setStatus('已加载 ' + entries.length + ' 个 Key', 'ok');
        if (!entries.length) resetForm(null);
      } catch (error) {
        setStatus(error.message || String(error), 'error');
        list.innerHTML = '<div class="cpa-empty">读取失败，请检查管理密钥（见下方“管理密钥”框）</div>';
      }
    }

    function resetForm(entry) {
      editingIndex = entry ? entry.index : null;
      formTitle.textContent = entry ? '编辑 Cursor API Key' : '新增 Cursor API Key';
      apiKeyInput.value = '';
      apiKeyInput.placeholder = entry ? '留空表示不更换现有 Key' : 'crsr_...';
      apiKeyHint.textContent = entry
        ? '当前 Key 已掩码；留空会保留原 Key。'
        : 'Key 只在保存时提交，列表不会返回明文。';
      var g = function (name) { return host.getElementsByClassName(name)[0]; };
      g('cpa-form-prefix').value = entry ? (entry.prefix || '') : '';
      g('cpa-form-priority').value = entry ? String(entry.priority || 0) : '0';
      g('cpa-form-baseurl').value = entry ? (entry['base-url'] || '') : '';
      g('cpa-form-excluded').value = entry && Array.isArray(entry['excluded-models'])
        ? entry['excluded-models'].join('\n')
        : '';
      g('cpa-form-cooling').checked = !!(entry && entry['disable-cooling']);
      g('cpa-form-disabled').checked = !!(entry && entry.disabled);
      form.classList.remove('cpa-hidden');
      form.scrollIntoView({ behavior: 'smooth', block: 'start' });
      apiKeyInput.focus();
    }

    function closeForm() {
      editingIndex = null;
      apiKeyInput.value = '';
      form.classList.add('cpa-hidden');
    }

    async function save(event) {
      event.preventDefault();
      var g = function (name) { return host.getElementsByClassName(name)[0]; };
      var rawKey = apiKeyInput.value.trim();
      if (editingIndex === null && !rawKey) {
        setStatus('新增时必须填写 Cursor API Key', 'error');
        apiKeyInput.focus();
        return;
      }
      var excluded = g('cpa-form-excluded').value
        .split(/\r?\n/)
        .map(function (value) { return value.trim(); })
        .filter(Boolean);
      var value = {
        'prefix': g('cpa-form-prefix').value.trim(),
        'priority': Number(g('cpa-form-priority').value || 0),
        'base-url': g('cpa-form-baseurl').value.trim(),
        'excluded-models': excluded,
        'disable-cooling': g('cpa-form-cooling').checked,
        'disabled': g('cpa-form-disabled').checked
      };
      if (rawKey) value['api-key'] = rawKey;

      setStatus('正在保存…');
      try {
        if (editingIndex === null) {
          await request('POST', '/v0/management/cursor-api-key', value);
        } else {
          await request('PATCH', '/v0/management/cursor-api-key', {
            index: editingIndex,
            value: value
          });
        }
        closeForm();
        await load();
        setStatus('保存成功，配置已热重载', 'ok');
      } catch (error) {
        setStatus(error.message || String(error), 'error');
      }
    }

    // 必须先构造 DOM，再绑定事件。旧版把 addEventListener 写在 querySelector 之前，
    // 空列表页会直接 TypeError，看起来像“没法填写”。
    var panel = document.createElement('div');
    panel.className = 'OAuthPage-module__container cpa-panel';
    panel.innerHTML =
      '<h1 class="OAuthPage-module__pageTitle">Cursor API Key</h1>' +
      '<div class="OAuthPage-module__content" style="width:100%">' +
      '<section class="OAuthPage-module__providerSection">' +
      '<h2 class="OAuthPage-module__sectionTitle">管理凭据</h2>' +
      '<div class="card">' +
      '<div class="form-group"><label for="cpa-management-key" style="color:var(--text-primary);font-weight:600">管理密钥（通常自动获取）</label>' +
      '<input id="cpa-management-key" class="form-control" type="password" autocomplete="off" placeholder="仅在自动获取失败时填写">' +
      '<p class="cpa-hint">不会写入配置或浏览器永久存储；刷新页面后清空。</p></div></div>' +
      '</section>' +
      '<section class="OAuthPage-module__providerSection">' +
      '<h2 class="OAuthPage-module__sectionTitle">API Key 列表</h2>' +
      '<div class="cpa-toolbar"><span class="cpa-status" role="status">等待加载</span>' +
      '<button type="button" class="btn" id="cpa-refresh">刷新</button>' +
      '<button type="button" class="btn btn-primary" id="cpa-add">新增 Key</button></div>' +
      '<div id="cpa-key-list" class="cpa-list"></div></section>' +
      '<section class="OAuthPage-module__providerSection">' +
      '<h2 class="OAuthPage-module__sectionTitle">新建 / 编辑</h2>' +
      '<form class="card cpa-form" id="cpa-form">' +
      '<div class="cpa-form-title" id="cpa-form-title">新增 Cursor API Key</div>' +
      '<div class="OAuthPage-module__oauthGrid">' +
      '<div class="cpa-field cpa-field--full"><label for="cpa-api-key">Cursor API Key</label>' +
      '<input id="cpa-api-key" class="form-control cpa-form-key" type="password" autocomplete="off" placeholder="crsr_...">' +
      '<p class="cpa-hint" id="cpa-api-key-hint">Key 只在保存时提交，列表不会返回明文。</p></div>' +
      '<div class="cpa-field"><label for="cpa-prefix">模型前缀</label>' +
      '<input id="cpa-prefix" class="form-control cpa-form-prefix" placeholder="留空，与 Cursor OAuth 共用模型">' +
      '<p class="cpa-hint">留空时共用同名模型、工具调用和故障切换；仅隔离账号时填写。</p></div>' +
      '<div class="cpa-field"><label for="cpa-priority">优先级</label>' +
      '<input id="cpa-priority" class="form-control cpa-form-priority" type="number" value="0" step="1"></div>' +
      '<div class="cpa-field cpa-field--full"><label for="cpa-base-url">Cursor API 地址</label>' +
      '<input id="cpa-base-url" class="form-control cpa-form-baseurl" type="url" placeholder="默认 https://api2.cursor.sh"></div>' +
      '<div class="cpa-field cpa-field--full"><label for="cpa-excluded-models">排除模型</label>' +
      '<textarea id="cpa-excluded-models" class="form-control cpa-form-excluded" placeholder="每行一个模型名"></textarea></div>' +
      '<div class="cpa-field cpa-field--full">' +
      '<label class="check" style="display:inline-flex;align-items:center;gap:7px;cursor:pointer"><input class="cpa-form-cooling" type="checkbox" style="width:auto">禁用冷却调度</label>' +
      '<label class="check" style="display:inline-flex;align-items:center;gap:7px;cursor:pointer"><input class="cpa-form-disabled" type="checkbox" style="width:auto">停用此 Key</label></div>' +
      '</div>' +
      '<div class="cpa-form-actions"><button type="button" class="btn cpa-form-cancel">取消</button>' +
      '<button type="button" class="btn btn-primary cpa-form-save">保存</button></div>' +
      '</form></section></div>' +
      '<style class="cpa-style">' +
      '.cpa-panel{display:block;width:100%;padding:8px 4px 24px;box-sizing:border-box}' +
      '.cpa-panel h1,.cpa-panel .OAuthPage-module__pageTitle{font-size:24px;margin:0 0 16px;color:var(--text-primary,#111)}' +
      '.cpa-panel .cpa-form{display:block;padding:16px;border:1px solid var(--border-color,#d4d4d8);border-radius:12px;background:var(--bg-elevated,transparent)}' +
      '.cpa-status{min-height:20px;font-size:13px;color:var(--text-secondary)}' +
      '.cpa-status--ok{color:#16a34a}.cpa-status--error{color:var(--danger,#c73e4e)}' +
      '.cpa-toolbar{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:6px}' +
      '.cpa-list{display:grid;gap:14px}' +
      '.cpa-hidden{display:none!important}' +
      '.cpa-empty{padding:26px;border:1px dashed var(--border-color);border-radius:12px;text-align:center;color:var(--text-secondary)}' +
      '.cpa-card-top{display:flex;flex-direction:column;gap:10px}' +
      '.cpa-key{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:14px;font-weight:700;word-break:break-all;color:var(--text-primary)}' +
      '.cpa-tags{display:flex;gap:7px;flex-wrap:wrap}' +
      '.cpa-tag{border:1px solid var(--border-color);border-radius:999px;padding:3px 9px;color:var(--text-secondary);font-size:12px}' +
      '.cpa-tag--ok{color:#16a34a;border-color:#22c55e55}.cpa-tag--off{color:var(--danger,#c73e4e);border-color:#ef444455}' +
      '.cpa-actions{display:flex;gap:9px;flex-wrap:wrap}' +
      '.cpa-runtime{display:flex;gap:16px;align-items:center;flex-wrap:wrap;margin-top:10px;padding-top:11px;border-top:1px solid var(--border-color);font-size:13px;color:var(--text-secondary)}' +
      '.cpa-metric b{font-variant-numeric:tabular-nums;color:var(--text-primary)}.cpa-metric--ok b{color:#16a34a}.cpa-metric--fail b{color:var(--danger,#c73e4e)}' +
      '.cpa-error-box{margin-top:12px;padding:11px 12px;border:1px solid #ef444455;border-radius:9px;background:#ef444410;font-size:13px;line-height:1.5;word-break:break-word}' +
      '.cpa-error-title{color:var(--danger,#c73e4e);font-weight:700;margin-bottom:4px}' +
      '.cpa-error-meta{display:flex;gap:12px;flex-wrap:wrap;margin-top:6px;color:var(--text-secondary);font-size:12px}' +
      '.cpa-form-title{font-size:16px;font-weight:700;color:var(--text-primary);margin:0 0 14px}' +
      '.cpa-field{display:flex;flex-direction:column;gap:5px}.cpa-field--full{grid-column:1/-1}' +
      '.cpa-field label{color:var(--text-primary);font-weight:600;font-size:13px}' +
      '.cpa-hint{margin:0;color:var(--text-secondary);font-size:12px;line-height:1.5}' +
      '.cpa-form-actions{display:flex;justify-content:flex-end;gap:9px;margin-top:16px}' +
      '.cpa-form .form-control{width:100%}' +
      '@media(max-width:640px){.cpa-actions{justify-content:flex-start}}</style>';

    host.appendChild(panel);
    status = panel.querySelector('.cpa-status');
    list = panel.querySelector('#cpa-key-list');
    form = panel.querySelector('#cpa-form');
    formTitle = panel.querySelector('#cpa-form-title');
    apiKeyInput = panel.querySelector('#cpa-api-key');
    apiKeyHint = panel.querySelector('#cpa-api-key-hint');
    managementKeyInput = panel.querySelector('#cpa-management-key');

    list.addEventListener('click', async function (event) {
      var button = event.target.closest('button[data-action]');
      if (!button) return;
      var index = Number(button.getAttribute('data-index'));
      var action = button.getAttribute('data-action');
      var entry = entries.find(function (item) { return item.index === index; });
      if (!entry) return;
      if (action === 'edit') {
        resetForm(entry);
        return;
      }
      if (action === 'delete') {
        if (!window.confirm('确定删除 ' + entry['api-key'] + ' 吗？此操作不可撤销。')) return;
        setStatus('正在删除…');
        try {
          await request('DELETE', '/v0/management/cursor-api-key?index=' + encodeURIComponent(index));
          closeForm();
          await load();
          setStatus('删除成功', 'ok');
        } catch (error) {
          setStatus(error.message || String(error), 'error');
        }
        return;
      }
      if (action === 'toggle') {
        setStatus(entry.disabled ? '正在启用…' : '正在停用…');
        try {
          await request('PATCH', '/v0/management/cursor-api-key', {
            index: index,
            value: { disabled: !entry.disabled }
          });
          await load();
          setStatus(entry.disabled ? '已启用' : '已停用', 'ok');
        } catch (error) {
          setStatus(error.message || String(error), 'error');
        }
      }
    });
    form.addEventListener('submit', save);
    panel.querySelector('.cpa-form-cancel').addEventListener('click', closeForm);
    panel.querySelector('.cpa-form-save').addEventListener('click', function () {
      form.dispatchEvent(new Event('submit', { cancelable: true, bubbles: true }));
    });
    panel.querySelector('#cpa-refresh').addEventListener('click', load);
    panel.querySelector('#cpa-add').addEventListener('click', function () { resetForm(null); });

    load();
    } catch (error) {
      host.__cpaMounted = false;
      host.textContent = 'Cursor API Key 管理界面加载失败：' + (error && error.message ? error.message : String(error));
    }
  }

  function ensureHost() {
    var host = document.getElementById('cpa-native-route-host');
    if (host) mount(host);
  }

  // 挂载点由上游路由渲染：进入 #/cursor-api-key 时出现、离开时被 React 移除。
  // React 只卸载自己持有的节点，原生挂载内容随之一起消失；每次再次进入时
  // 由监听器重新检测并挂载。
  function start() {
    ensureHost();
    if (window.MutationObserver) {
      try {
        new MutationObserver(function () { ensureHost(); })
          .observe(document.documentElement, { childList: true, subtree: true });
      } catch (_) {}
    }
    setInterval(ensureHost, 800);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start, { once: true });
  } else {
    start();
  }
})();
</script>`
