from pathlib import Path

t = Path(__file__).resolve().parents[1].joinpath("static", "management.html").read_text(encoding="utf-8")
start = 0
for i in range(4):
    idx = t.find("xai_oauth_polling_error:`", start)
    if idx < 0:
        break
    snippet = t[idx : idx + 120]
    print(i, snippet.encode("unicode_escape").decode())
    start = idx + 1
