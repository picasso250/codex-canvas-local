#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import asdict, dataclass
from urllib.request import urlopen

from playwright.sync_api import sync_playwright


DEFAULT_CDP_ENDPOINT = "http://127.0.0.1:9222"
DEFAULT_URL = "https://chatgpt.com/codex/cloud/settings/analytics#usage"


@dataclass
class UsageLimit:
    name: str
    remaining_percent: int | None
    reset_time: str | None


def resolve_ws_endpoint(cdp_endpoint: str) -> str:
    with urlopen(f"{cdp_endpoint.rstrip('/')}/json/version", timeout=5) as response:
        payload = json.load(response)
    ws_endpoint = payload.get("webSocketDebuggerUrl")
    if not ws_endpoint:
        raise RuntimeError(f"Missing webSocketDebuggerUrl from {cdp_endpoint}")
    return ws_endpoint


def find_or_open_page(browser, target_url: str):
    for context in browser.contexts:
        for page in context.pages:
            if page.url == target_url:
                return page

    for context in browser.contexts:
        for page in context.pages:
            if page.url.startswith("https://chatgpt.com/codex/cloud/settings/analytics"):
                page.goto(target_url, wait_until="domcontentloaded")
                page.wait_for_timeout(3500)
                return page

    if not browser.contexts:
        raise RuntimeError("No browser contexts found in the CDP session")

    page = browser.contexts[0].new_page()
    page.goto(target_url, wait_until="domcontentloaded")
    page.wait_for_timeout(3500)
    return page


def parse_limits(text: str) -> dict[str, object]:
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    limits: list[UsageLimit] = []

    for index, line in enumerate(lines):
        inline_match = re.fullmatch(r"(.+?使用限额)\s+(\d+)%\s+剩余", line)
        split_match = re.fullmatch(r"(.+?使用限额)", line)
        if inline_match:
            name = inline_match.group(1)
            remaining_percent = int(inline_match.group(2))
        elif split_match and index + 2 < len(lines):
            percent_match = re.fullmatch(r"(\d+)%", lines[index + 1])
            if not percent_match or lines[index + 2] != "剩余":
                continue
            name = split_match.group(1)
            remaining_percent = int(percent_match.group(1))
        else:
            continue

        reset_time = None
        for follow in lines[index + 1 : index + 5]:
            if follow.startswith("重置时间："):
                reset_time = follow.removeprefix("重置时间：").strip()
                break

        limits.append(
            UsageLimit(
                name=name,
                remaining_percent=remaining_percent,
                reset_time=reset_time,
            )
        )

    credit_balance = None
    for index, line in enumerate(lines):
        if line == "剩余额度" and index + 1 < len(lines):
            next_line = lines[index + 1]
            if re.fullmatch(r"\d+", next_line):
                credit_balance = int(next_line)
                break

    turns = None
    for index, line in enumerate(lines):
        if line == "Turns" and index + 1 < len(lines):
            next_line = lines[index + 1].replace(",", "")
            if re.fullmatch(r"\d+", next_line):
                turns = int(next_line)
                break

    return {
        "limits": [asdict(limit) for limit in limits],
        "credit_balance": credit_balance,
        "turns": turns,
    }


def read_usage(cdp_endpoint: str, url: str) -> dict[str, object]:
    ws_endpoint = resolve_ws_endpoint(cdp_endpoint)
    with sync_playwright() as playwright:
        browser = playwright.chromium.connect_over_cdp(ws_endpoint)
        try:
            page = find_or_open_page(browser, url)
            page.wait_for_timeout(1000)
            text = page.locator("body").inner_text(timeout=10000)
            result = parse_limits(text)
            result["url"] = page.url
            result["title"] = page.title()
            return result
        finally:
            browser.close()


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description="Read Codex usage limits from the logged-in ChatGPT tab.")
    parser.add_argument("--cdp-endpoint", default=DEFAULT_CDP_ENDPOINT)
    parser.add_argument("--url", default=DEFAULT_URL)
    parser.add_argument("--json", action="store_true", help="Print machine-readable JSON.")
    args = parser.parse_args(argv)

    result = read_usage(args.cdp_endpoint, args.url)
    if args.json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
        return 0

    print(f"{result['title']} - {result['url']}")
    for limit in result["limits"]:
        reset = limit["reset_time"] or "unknown"
        print(f"{limit['name']}: {limit['remaining_percent']}% 剩余，重置时间：{reset}")
    print(f"剩余额度: {result['credit_balance']}")
    print(f"Turns: {result['turns']}")
    return 0


if __name__ == "__main__":
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    sys.exit(main(sys.argv[1:]))
