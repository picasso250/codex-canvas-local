#!/usr/bin/env python3
import argparse
import asyncio

from playwright.async_api import async_playwright


async def restore(cdp_url: str) -> None:
    async with async_playwright() as playwright:
        browser = await playwright.chromium.connect_over_cdp(cdp_url)
        session = await browser.new_browser_cdp_session()
        try:
            await session.send("Browser.setDownloadBehavior", {"behavior": "default"})
        finally:
            await session.detach()


def main() -> None:
    parser = argparse.ArgumentParser(description="Restore Chromium's normal download behavior.")
    parser.add_argument("--cdp-url", default="http://127.0.0.1:9222")
    args = parser.parse_args()
    asyncio.run(restore(args.cdp_url))
    print("Browser downloads restored to Chromium's default directory.")


if __name__ == "__main__":
    main()
