from __future__ import annotations

import re
from pathlib import Path

p = Path(__file__).resolve().parents[1] / "static" / "management.html"
text = p.read_text(encoding="utf-8")

old_tk = (
    "var tk=[{kind:`builtin`,id:`codex`,titleKey:`auth_login.codex_oauth_title`,icon:Bb},"
    "{kind:`builtin`,id:`anthropic`,titleKey:`auth_login.anthropic_oauth_title`,icon:zb},"
    "{kind:`builtin`,id:`antigravity`,titleKey:`auth_login.antigravity_oauth_title`,icon:pT},"
    "{kind:`builtin`,id:`kimi`,titleKey:`auth_login.kimi_oauth_title`,icon:{light:vT,dark:_T}},"
    "{kind:`builtin`,id:`xai`,titleKey:`auth_login.xai_oauth_title`,icon:{light:mT,dark:hT}}]"
)
new_tk = (
    "var tk=[{kind:`builtin`,id:`codex`,titleKey:`auth_login.codex_oauth_title`,icon:Bb},"
    "{kind:`builtin`,id:`anthropic`,titleKey:`auth_login.anthropic_oauth_title`,icon:zb},"
    "{kind:`builtin`,id:`antigravity`,titleKey:`auth_login.antigravity_oauth_title`,icon:pT},"
    "{kind:`builtin`,id:`kimi`,titleKey:`auth_login.kimi_oauth_title`,icon:{light:vT,dark:_T}},"
    "{kind:`builtin`,id:`xai`,titleKey:`auth_login.xai_oauth_title`,icon:{light:mT,dark:hT}},"
    "{kind:`builtin`,id:`cursor`,titleKey:`auth_login.cursor_oauth_title`,icon:{light:mT,dark:hT}}]"
)
if "id:`cursor`" not in text:
    if old_tk not in text:
        raise SystemExit("tk array not found")
    text = text.replace(old_tk, new_tk, 1)

locale_patches = [
    (
        "xai_oauth_polling_error:`轮询认证状态失败:`,plugin_oauth_title:`{{name}} OAuth`",
        "xai_oauth_polling_error:`轮询认证状态失败:`,"
        "cursor_oauth_title:`Cursor OAuth`,"
        "cursor_oauth_button:`开始 Cursor 登录`,"
        "cursor_oauth_hint:`通过 Cursor 深度控制授权登录，自动获取并保存认证文件。`,"
        "cursor_oauth_url_label:`授权链接:`,"
        "cursor_open_link:`打开链接`,"
        "cursor_copy_link:`复制链接`,"
        "cursor_oauth_status_waiting:`等待认证中...`,"
        "cursor_oauth_status_success:`认证成功！`,"
        "cursor_oauth_status_error:`认证失败:`,"
        "cursor_oauth_start_error:`启动 Cursor OAuth 失败:`,"
        "cursor_oauth_polling_error:`轮询认证状态失败:`,"
        "plugin_oauth_title:`{{name}} OAuth`",
    ),
    (
        "xai_oauth_polling_error:`輪詢認證狀態失敗:`,plugin_oauth_title:`{{name}} OAuth`",
        "xai_oauth_polling_error:`輪詢認證狀態失敗:`,"
        "cursor_oauth_title:`Cursor OAuth`,"
        "cursor_oauth_button:`開始 Cursor 登入`,"
        "cursor_oauth_hint:`透過 Cursor 深度控制授權登入，自動取得並儲存認證檔案。`,"
        "cursor_oauth_url_label:`授權連結:`,"
        "cursor_open_link:`開啟連結`,"
        "cursor_copy_link:`複製連結`,"
        "cursor_oauth_status_waiting:`等待認證中...`,"
        "cursor_oauth_status_success:`認證成功！`,"
        "cursor_oauth_status_error:`認證失敗:`,"
        "cursor_oauth_start_error:`啟動 Cursor OAuth 失敗:`,"
        "cursor_oauth_polling_error:`輪詢認證狀態失敗:`,"
        "plugin_oauth_title:`{{name}} OAuth`",
    ),
    (
        "xai_oauth_polling_error:`Failed to check authentication status:`,plugin_oauth_title:`{{name}} OAuth`",
        "xai_oauth_polling_error:`Failed to check authentication status:`,"
        "cursor_oauth_title:`Cursor OAuth`,"
        "cursor_oauth_button:`Start Cursor Login`,"
        "cursor_oauth_hint:`Login to Cursor via deep-control authorization and automatically save authentication files.`,"
        "cursor_oauth_url_label:`Authorization URL:`,"
        "cursor_open_link:`Open Link`,"
        "cursor_copy_link:`Copy Link`,"
        "cursor_oauth_status_waiting:`Waiting for authentication...`,"
        "cursor_oauth_status_success:`Authentication successful!`,"
        "cursor_oauth_status_error:`Authentication failed:`,"
        "cursor_oauth_start_error:`Failed to start Cursor OAuth:`,"
        "cursor_oauth_polling_error:`Failed to check authentication status:`,"
        "plugin_oauth_title:`{{name}} OAuth`",
    ),
]

for old, *new_parts in locale_patches:
    new = "".join(new_parts)
    if old in text and "cursor_oauth_title:`" not in text[text.find(old) : text.find(old) + 200]:
        text = text.replace(old, new, 1)
        print("patched locale:", old[:48])

# Patch any remaining locales that still lack cursor keys after xai polling error.
fallback = (
    "cursor_oauth_title:`Cursor OAuth`,"
    "cursor_oauth_button:`Start Cursor Login`,"
    "cursor_oauth_hint:`Login to Cursor via deep-control authorization and automatically save authentication files.`,"
    "cursor_oauth_url_label:`Authorization URL:`,"
    "cursor_open_link:`Open Link`,"
    "cursor_copy_link:`Copy Link`,"
    "cursor_oauth_status_waiting:`Waiting for authentication...`,"
    "cursor_oauth_status_success:`Authentication successful!`,"
    "cursor_oauth_status_error:`Authentication failed:`,"
    "cursor_oauth_start_error:`Failed to start Cursor OAuth:`,"
    "cursor_oauth_polling_error:`Failed to check authentication status:`,"
)

parts = text.split("xai_oauth_polling_error:`")
out = [parts[0]]
for seg in parts[1:]:
    chunk = "xai_oauth_polling_error:`" + seg
    head = chunk[:500]
    if "cursor_oauth_title:" in head:
        out.append(chunk)
        continue
    m = re.match(r"(xai_oauth_polling_error:`[^`]*`,)(plugin_oauth_title:)", chunk)
    if not m:
        out.append(chunk)
        print("skip segment:", chunk[:70])
        continue
    out.append(m.group(1) + fallback + m.group(2) + chunk[m.end() :])
    print("patched remaining locale")
text = "".join(out)

p.write_text(text, encoding="utf-8")
print("cursor in tk:", "id:`cursor`" in text)
print("cursor_oauth_title count:", text.count("cursor_oauth_title:`"))
