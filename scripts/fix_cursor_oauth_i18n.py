from pathlib import Path
import re

p = Path(__file__).resolve().parents[1] / "static" / "management.html"
text = p.read_text(encoding="utf-8")

locales = {
    "检查认证状态失败:": {
        "cursor_oauth_title": "Cursor OAuth",
        "cursor_oauth_button": "开始 Cursor 登录",
        "cursor_oauth_hint": "通过 Cursor 深度控制授权登录，自动获取并保存认证文件。",
        "cursor_oauth_url_label": "授权链接:",
        "cursor_open_link": "打开链接",
        "cursor_copy_link": "复制链接",
        "cursor_oauth_status_waiting": "等待认证中...",
        "cursor_oauth_status_success": "认证成功！",
        "cursor_oauth_status_error": "认证失败:",
        "cursor_oauth_start_error": "启动 Cursor OAuth 失败:",
        "cursor_oauth_polling_error": "检查认证状态失败:",
    },
    "檢查驗證狀態失敗:": {
        "cursor_oauth_title": "Cursor OAuth",
        "cursor_oauth_button": "開始 Cursor 登入",
        "cursor_oauth_hint": "透過 Cursor 深度控制授權登入，自動取得並儲存認證檔案。",
        "cursor_oauth_url_label": "授權連結:",
        "cursor_open_link": "開啟連結",
        "cursor_copy_link": "複製連結",
        "cursor_oauth_status_waiting": "等待認證中...",
        "cursor_oauth_status_success": "認證成功！",
        "cursor_oauth_status_error": "認證失敗:",
        "cursor_oauth_start_error": "啟動 Cursor OAuth 失敗:",
        "cursor_oauth_polling_error": "檢查驗證狀態失敗:",
    },
}


def build_block(locale: dict[str, str]) -> str:
    return "".join(f"{k}:`{v}`," for k, v in locale.items())


for marker, locale in locales.items():
    pat = rf"(xai_oauth_polling_error:`{re.escape(marker)}`,)cursor_oauth_title:`[^`]*`,cursor_oauth_button:`[^`]*`,cursor_oauth_hint:`[^`]*`,cursor_oauth_url_label:`[^`]*`,cursor_open_link:`[^`]*`,cursor_copy_link:`[^`]*`,cursor_oauth_status_waiting:`[^`]*`,cursor_oauth_status_success:`[^`]*`,cursor_oauth_status_error:`[^`]*`,cursor_oauth_start_error:`[^`]*`,cursor_oauth_polling_error:`[^`]*`,"
    m = re.search(pat, text)
    if not m:
        print("not found:", marker)
        continue
    text = text[: m.start()] + m.group(1) + build_block(locale) + text[m.end() :]
    print("updated:", marker)

p.write_text(text, encoding="utf-8")
print("done")
