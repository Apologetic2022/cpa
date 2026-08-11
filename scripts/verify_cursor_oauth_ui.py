from pathlib import Path
import re

t = Path(__file__).resolve().parents[1].joinpath("static", "management.html").read_text(encoding="utf-8")
buttons = re.findall(r"cursor_oauth_button:`([^`]*)`", t)
print("buttons:", buttons)
print("has cursor id:", "id:`cursor`" in t)
print("zh button:", any("Cursor" in b and ("开始" in b or "開始" in b or "Start" in b) for b in buttons)
)
