"""Browser regression for Chat delivery. Run against the local production UI:
/opt/homebrew/bin/python3.11 scripts/verify_chat_live.py
Requires Playwright and Chrome. All API requests use synthetic responses.
"""

import json
import os
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import sync_playwright

base_url = os.environ.get("ARTEX_TEST_BASE_URL", "http://127.0.0.1:3000").rstrip("/")
assert urlparse(base_url).hostname in ("127.0.0.1", "localhost"), (
    "Use a local test server"
)
state = {"items": [], "running": False, "tails": 0}
failures = []


def item(seq, kind, text):
    return {
        "seq": seq,
        "kind": kind,
        "summary": text,
        "worker": "mainagent",
        "ts": "2026-10-19T00:00:00Z",
    }


with sync_playwright() as p:
    browser = p.chromium.launch(channel="chrome", headless=True)
    context = browser.new_context(viewport={"width": 1400, "height": 900})
    # Isolated test profile; every API request is fulfilled locally, never authenticated against ARTEX.
    context.add_cookies(
        [{"name": "artex_token", "value": "fixture-only", "url": base_url}]
    )
    context.add_init_script("localStorage.setItem('artex_token','fixture-only')")
    page = context.new_page()
    errors = []
    paths = []
    page.on("pageerror", lambda e: errors.append(str(e)))

    def route(r):
        u = urlparse(r.request.url)
        path = u.path
        q = parse_qs(u.query)
        paths.append(path)
        result = {}
        if path == "/api/conversations":
            result = {
                "conversations": [
                    {
                        "id": 1,
                        "title": "Fixture conversation",
                        "agent_key": "auto",
                        "running": state["running"],
                    }
                ]
            }
        elif path == "/api/agents":
            result = {
                "agents": [
                    {
                        "id": 1,
                        "key": "auto",
                        "name": "Auto",
                        "role": "assistant",
                        "builtin": True,
                        "enabled": True,
                    }
                ]
            }
        elif path == "/api/llm/profiles":
            result = {"profiles": []}
        elif path == "/api/conversations/1/messages":
            if r.request.method == "POST":
                state["items"].append(item(1, "user", "test question"))
                state["running"] = True
                result = {"status": "accepted"}
            else:
                since = int(q.get("since", ["0"])[0])
                rows = [i for i in state["items"] if i["seq"] > since]
                if "since" in q:
                    state["tails"] += 1
                result = {
                    "items": rows,
                    "cursor": state["items"][-1]["seq"] if state["items"] else 0,
                    "running": state["running"],
                    "hasMore": False,
                }
        elif path.startswith("/api/conversations/1/messages/"):
            seq = int(path.rsplit("/", 1)[-1])
            result = {
                "detail": ("Answer paragraph.\n\n" * 100 + "END OF ANSWER")
                if seq == 2
                else next((i["summary"] for i in state["items"] if i["seq"] == seq), "")
            }
        r.fulfill(status=200, content_type="application/json", body=json.dumps(result))

    context.route("**/api/**", route)
    page.goto(base_url + "/chat?c=1", wait_until="networkidle")
    print(
        "initial",
        json.dumps(
            {
                "errors": errors,
                "paths": sorted(set(paths)),
                "textareas": page.locator("textarea").count(),
            }
        ),
    )
    page.locator("textarea").fill("test question")
    page.locator("textarea").press("Enter")
    page.wait_for_timeout(1600)
    state["items"].append(item(2, "result", "SHORT ANSWER"))
    state["running"] = False
    page.wait_for_timeout(3500)
    print(
        "ownsend",
        json.dumps(
            {
                "tails": state["tails"],
                "answer_in_dom": page.get_by_text("END OF ANSWER", exact=False).count(),
                "scrolls": page.locator(
                    '[data-slot="scroll-area-viewport"]'
                ).evaluate_all(
                    "(els)=>els.map(e=>({top:e.scrollTop,height:e.clientHeight,total:e.scrollHeight}))"
                ),
                "errors": errors,
            }
        ),
    )
    if not page.get_by_text("END OF ANSWER", exact=False).count():
        failures.append("Submitted turn answer missing from DOM")
    scroll = page.locator('[data-slot="scroll-area-viewport"]').last
    gap = scroll.evaluate("(e)=>e.scrollHeight-e.clientHeight-e.scrollTop")
    if gap > 60:
        failures.append(f"Expanded answer is hidden below viewport: bottom gap {gap}px")
    # Reading old messages must not be interrupted by subsequent updates.
    scroll.evaluate('(e)=>{e.scrollTop=150;e.dispatchEvent(new Event("scroll"))}')
    state["items"].append(item(3, "text", "MISSED BACKGROUND ANSWER"))
    page.wait_for_timeout(6000)
    print(
        "background",
        json.dumps(
            {
                "tails": state["tails"],
                "visible": page.get_by_text(
                    "MISSED BACKGROUND ANSWER", exact=False
                ).count(),
            }
        ),
    )
    if not page.get_by_text("MISSED BACKGROUND ANSWER", exact=False).count():
        failures.append("Idle conversation did not receive background answer")
    if abs(scroll.evaluate("(e)=>e.scrollTop") - 150) > 10:
        failures.append("New reply interrupted reading older messages")
    if errors:
        failures.extend(errors)
    browser.close()

assert not failures, "\n".join(failures)
print(
    "PASS: long answer follows viewport; idle delivery recovers; reading position preserved"
)
