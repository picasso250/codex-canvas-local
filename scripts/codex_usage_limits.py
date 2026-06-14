#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import re
import sys
import time
from dataclasses import asdict, dataclass
from typing import Any
from urllib.parse import quote
from urllib.request import Request
from urllib.request import urlopen

import websocket


DEFAULT_CDP_ENDPOINT = "http://127.0.0.1:9222"
DEFAULT_URL = "https://chatgpt.com/codex/cloud/settings/analytics#usage"


@dataclass
class UsageLimit:
    name: str
    remaining_percent: int | None
    reset_time: str | None


def fetch_json(cdp_endpoint: str, path: str, timeout: float = 5) -> Any:
    if not path.startswith("/"):
        path = f"/{path}"
    request = Request(
        f"{cdp_endpoint.rstrip('/')}{path}",
        headers={"User-Agent": "codex-canvas-local/usage-limits"},
    )
    with urlopen(request, timeout=timeout) as response:
        return json.load(response)


def fetch_targets(cdp_endpoint: str) -> list[dict[str, Any]]:
    payload = fetch_json(cdp_endpoint, "/json/list")
    if not isinstance(payload, list):
        raise RuntimeError("Unexpected CDP /json/list response")
    return [target for target in payload if target.get("type") == "page"]


def open_page(cdp_endpoint: str, target_url: str) -> dict[str, Any]:
    path = f"/json/new?{quote(target_url, safe=':/?&=%')}"
    request = Request(
        f"{cdp_endpoint.rstrip('/')}{path}",
        method="PUT",
        headers={"User-Agent": "codex-canvas-local/usage-limits"},
    )
    with urlopen(request, timeout=10) as response:
        payload = json.load(response)
    if not isinstance(payload, dict):
        raise RuntimeError("Unexpected CDP /json/new response")
    return payload


def find_or_open_target(cdp_endpoint: str, target_url: str) -> dict[str, Any]:
    targets = fetch_targets(cdp_endpoint)
    for target in targets:
        if target.get("url") == target_url:
            return target

    target_prefix = "https://chatgpt.com/codex/cloud/settings/analytics"
    for target in targets:
        if str(target.get("url", "")).startswith(target_prefix):
            evaluate_target(target, f"location.href = {json.dumps(target_url)}")
            return target

    return open_page(cdp_endpoint, target_url)


def evaluate_target(target: dict[str, Any], expression: str) -> Any:
    ws_url = target.get("webSocketDebuggerUrl")
    if not ws_url:
        raise RuntimeError(f"Missing page webSocketDebuggerUrl for {target.get('url', '<unknown>')}")

    message = {
        "id": 1,
        "method": "Runtime.evaluate",
        "params": {
            "expression": expression,
            "returnByValue": True,
            "awaitPromise": True,
        },
    }
    sock = websocket.create_connection(ws_url, timeout=15, suppress_origin=True)
    try:
        sock.send(json.dumps(message))
        while True:
            response = json.loads(sock.recv())
            if response.get("id") != 1:
                continue
            if "error" in response:
                raise RuntimeError(response["error"].get("message", "CDP evaluation failed"))
            result = response.get("result", {}).get("result", {})
            if "exceptionDetails" in response.get("result", {}):
                raise RuntimeError("CDP evaluation raised an exception")
            return result.get("value")
    finally:
        sock.close()


def read_page_payload(target: dict[str, Any]) -> dict[str, Any]:
    payload = evaluate_target(
        target,
        """
        ({
          url: location.href,
          title: document.title,
          text: document.body ? document.body.innerText : "",
          readyState: document.readyState
        })
        """,
    )
    if not isinstance(payload, dict):
        raise RuntimeError("Unexpected CDP Runtime.evaluate result")
    return payload


def wait_for_usage_page(target: dict[str, Any], target_url: str, timeout: float = 20) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last_payload: dict[str, Any] | None = None
    target_prefix = target_url.split("#", 1)[0]

    while time.monotonic() < deadline:
        payload = read_page_payload(target)
        last_payload = payload
        current_url = str(payload.get("url", ""))
        text = str(payload.get("text", ""))
        ready_state = payload.get("readyState")
        if (
            current_url.startswith(target_prefix)
            and ready_state in {"interactive", "complete"}
            and ("使用限额" in text or "Codex 分析" in text)
        ):
            return payload
        time.sleep(1)

    if last_payload is not None:
        return last_payload
    raise RuntimeError("Timed out waiting for usage page")


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
    target = find_or_open_target(cdp_endpoint, url)
    payload = wait_for_usage_page(target, url)
    text = str(payload.get("text", ""))
    result = parse_limits(text)
    result["url"] = payload.get("url") or target.get("url")
    result["title"] = payload.get("title") or target.get("title")
    return result


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
