import asyncio
import importlib.util
import json
import sys
import unittest
from unittest.mock import AsyncMock, MagicMock, patch
from pathlib import Path

MODULE_PATH = Path(__file__).with_name("chatgpt_agent.py")
SPEC = importlib.util.spec_from_file_location("chatgpt_agent", MODULE_PATH)
assert SPEC and SPEC.loader
agent = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = agent
SPEC.loader.exec_module(agent)


class BrowserEndpointTests(unittest.TestCase):
    def test_reads_browser_websocket_from_json_version(self):
        response = MagicMock()
        response.read.return_value = json.dumps(
            {"webSocketDebuggerUrl": "ws://localhost:9222/devtools/browser/test-browser-id"}
        ).encode("utf-8")
        response.__enter__.return_value = response

        with patch.object(agent, "urlopen", return_value=response) as open_mock:
            self.assertEqual(
                agent.browser_ws_endpoint("http://127.0.0.1:9222"),
                "ws://127.0.0.1:9222/devtools/browser/test-browser-id",
            )
        open_mock.assert_called_once_with("http://127.0.0.1:9222/json/version", timeout=5)

    def test_rejects_json_version_without_browser_websocket(self):
        response = MagicMock()
        response.read.return_value = b"{}"
        response.__enter__.return_value = response

        with patch.object(agent, "urlopen", return_value=response):
            with self.assertRaises(agent.AgentError) as raised:
                agent.browser_ws_endpoint("http://127.0.0.1:9222")

        self.assertEqual(raised.exception.code, "cdp_missing_ws")


class ConversationUrlTests(unittest.IsolatedAsyncioTestCase):
    def test_only_accepts_persisted_conversation_route(self):
        self.assertTrue(agent.is_conversation_url("https://chatgpt.com/c/test-id"))
        self.assertTrue(agent.is_conversation_url("https://chatgpt.com/c/test-id?model=gpt-5"))
        self.assertFalse(agent.is_conversation_url("https://chatgpt.com/"))
        self.assertFalse(agent.is_conversation_url("https://chatgpt.com/web:test-id"))

    async def test_ignores_temporary_route_and_returns_conversation_url(self):
        page = FakePage("https://chatgpt.com/")
        observed: list[str] = []

        async def send_action() -> None:
            page.emit_navigation("https://chatgpt.com/web:test-id")
            page.url = "https://chatgpt.com/c/test-id"
            page.emit_navigation(page.url)
            page.url = "https://chatgpt.com/"

        result = await agent.wait_for_conversation_url(page, send_action, observed.append)

        self.assertEqual(result, "https://chatgpt.com/c/test-id")
        self.assertEqual(page.url, "https://chatgpt.com/")
        self.assertEqual(
            observed,
            [
                "https://chatgpt.com/",
                "https://chatgpt.com/web:test-id",
                "https://chatgpt.com/c/test-id",
            ],
        )


class PlaywrightLifecycleTests(unittest.IsolatedAsyncioTestCase):
    async def test_stop_uses_started_playwright_object(self):
        chatgpt_agent = agent.ChatGPTAgent("https://chatgpt.com/")
        started_playwright = MagicMock()
        started_playwright.stop = AsyncMock()
        chatgpt_agent.playwright_manager = MagicMock()
        chatgpt_agent.playwright = started_playwright

        await chatgpt_agent.stop()

        started_playwright.stop.assert_awaited_once_with()
        self.assertIsNone(chatgpt_agent.playwright_manager)
        self.assertIsNone(chatgpt_agent.playwright)

    async def test_reset_stops_old_playwright_and_starts_a_new_one(self):
        chatgpt_agent = agent.ChatGPTAgent("https://chatgpt.com/")
        old_playwright = MagicMock()
        old_playwright.stop = AsyncMock()
        new_playwright = MagicMock()
        new_manager = MagicMock()
        new_manager.start = AsyncMock(return_value=new_playwright)
        chatgpt_agent.playwright = old_playwright

        with patch.object(agent, "async_playwright", return_value=new_manager):
            await chatgpt_agent.reset_browser()

        old_playwright.stop.assert_awaited_once_with()
        new_manager.start.assert_awaited_once_with()
        self.assertIs(chatgpt_agent.playwright_manager, new_manager)
        self.assertIs(chatgpt_agent.playwright, new_playwright)


class ImagegenStateReadyTests(unittest.TestCase):
    def complete_state(self):
        return {
            "has_images": True,
            "images_loaded": True,
            "image_urls": ["https://example.test/image.png"],
            "has_preview": False,
        }

    def test_accepts_completed_image_turn(self):
        self.assertTrue(agent.imagegen_state_ready(self.complete_state()))

    def test_rejects_preview_phase(self):
        state = self.complete_state()
        state["has_preview"] = True
        self.assertFalse(agent.imagegen_state_ready(state))

    def test_accepts_when_chatgpt_controls_are_stuck(self):
        state = self.complete_state()
        state["has_reply_actions"] = False
        state["is_streaming"] = True
        state["has_stop_button"] = True
        self.assertTrue(agent.imagegen_state_ready(state))

    def test_requires_loaded_images(self):
        for key in ("has_images", "images_loaded"):
            with self.subTest(key=key):
                state = self.complete_state()
                state[key] = False
                self.assertFalse(agent.imagegen_state_ready(state))


class FakePage:
    def __init__(self, url: str) -> None:
        self.url = url
        self.closed = False
        self.visited_url = None
        self.main_frame = self
        self.listeners: dict[str, list] = {}

    def on(self, event: str, handler) -> None:
        self.listeners.setdefault(event, []).append(handler)

    def remove_listener(self, event: str, handler) -> None:
        self.listeners[event].remove(handler)

    def emit_navigation(self, url: str) -> None:
        self.url = url
        for handler in list(self.listeners.get("framenavigated", [])):
            handler(self)

    async def evaluate(self, _expression, *_args):
        return {"visibility": "visible", "focused": True}

    async def close(self) -> None:
        self.closed = True

    async def goto(self, url: str) -> None:
        self.visited_url = url
        self.url = url

    async def bring_to_front(self) -> None:
        return None


class FakeContext:
    def __init__(self, recovery_page: FakePage) -> None:
        self.recovery_page = recovery_page

    async def new_page(self) -> FakePage:
        return self.recovery_page


class ImageUploadPromptFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_types_prompt_before_waiting_for_upload_completion(self):
        events: list[str] = []
        page = FakePage("about:blank")
        context = FakeContext(page)
        browser = type("FakeBrowser", (), {"contexts": [context]})()
        chatgpt_agent = agent.ChatGPTAgent("https://chatgpt.com/")
        chatgpt_agent.ensure_browser = AsyncMock(return_value=browser)
        chatgpt_agent.start_image_upload = AsyncMock(
            side_effect=lambda *_: events.append("start_upload")
        )
        chatgpt_agent.wait_for_image_upload = AsyncMock(
            side_effect=lambda *_: events.append("wait_upload")
        )
        chatgpt_agent.wait_for_imagegen_with_tab_recovery = AsyncMock(
            side_effect=lambda *_: (events.append("wait_generation") or (page, "done"))
        )
        chatgpt_agent.download_images = AsyncMock(
            side_effect=lambda *_: (events.append("download") or [])
        )
        future = asyncio.get_running_loop().create_future()
        job = agent.Job(
            request_id="request-1",
            prompt="draw a cat",
            stable_seconds=5.0,
            images=["reference.png"],
            workdir="",
            future=future,
        )

        async def record_sleep(seconds: float) -> None:
            events.append(f"sleep:{seconds}")

        async def record_click(_page, selector: str) -> None:
            if selector == "#prompt-textarea":
                events.append("click_prompt")
            else:
                events.append("click_send")

        async def record_type(_page, text: str) -> None:
            self.assertEqual(text, "draw a cat")
            events.append("type_prompt")

        async def record_url_wait(_page, send_action, _on_url) -> str:
            events.append("wait_conversation_url")
            await send_action()
            return "https://chatgpt.com/c/test-id"

        with (
            patch.object(agent, "stable_wait", new=AsyncMock()),
            patch.object(agent.asyncio, "sleep", new=record_sleep),
            patch.object(agent, "turn_count", new=AsyncMock(return_value=0)),
            patch.object(agent, "assistant_messages", new=AsyncMock(return_value=[])),
            patch.object(agent, "click_element_center", new=record_click),
            patch.object(agent, "type_like_user", new=record_type),
            patch.object(
                agent,
                "wait_for_conversation_url",
                new=AsyncMock(side_effect=record_url_wait),
            ),
            patch.object(chatgpt_agent, "report_progress"),
        ):
            result = await chatgpt_agent.handle_ask_once(job)

        self.assertTrue(result["ok"])
        self.assertEqual(
            events,
            [
                "sleep:1.0",
                "start_upload",
                "sleep:0.1",
                "click_prompt",
                "type_prompt",
                "wait_upload",
                "sleep:0.1",
                "wait_conversation_url",
                "click_send",
                "wait_generation",
                "sleep:0.1",
                "download",
            ],
        )


class ImagegenRecoveryTests(unittest.IsolatedAsyncioTestCase):
    async def test_second_wait_is_capped_at_120_seconds_and_falls_back_to_images(self):
        original_page = FakePage("https://chatgpt.com/c/test")
        recovery_page = FakePage("about:blank")
        context = FakeContext(recovery_page)
        browser = type("FakeBrowser", (), {"contexts": [context]})()
        chatgpt_agent = agent.ChatGPTAgent("https://chatgpt.com/")
        chatgpt_agent.ensure_browser = AsyncMock(return_value=browser)
        timeout = agent.AgentError("response_timeout", "timeout")

        with (
            patch.object(agent, "wait_for_imagegen", new=AsyncMock(side_effect=[timeout, timeout])) as wait_mock,
            patch.object(agent, "stable_wait", new=AsyncMock()),
            patch.object(
                agent,
                "imagegen_state",
                new=AsyncMock(return_value={"image_urls": ["https://example.test/image.png"]}),
            ),
            patch.object(agent, "get_text_response", new=AsyncMock(return_value="partial response")),
        ):
            page, response = await chatgpt_agent.wait_for_imagegen_with_tab_recovery(
                original_page,
                before_turn_count=0,
                before_assistant_count=0,
                conversation_url="https://chatgpt.com/c/test",
            )

        self.assertIs(page, recovery_page)
        self.assertEqual(response, "partial response")
        self.assertTrue(original_page.closed)
        self.assertEqual(recovery_page.visited_url, "https://chatgpt.com/c/test")
        self.assertEqual(wait_mock.await_args_list[0].args[-1], 60.0)
        self.assertEqual(wait_mock.await_args_list[1].args[-1], 120.0)

    async def test_final_timeout_still_fails_without_any_image_url(self):
        original_page = FakePage("https://chatgpt.com/c/test")
        recovery_page = FakePage("about:blank")
        context = FakeContext(recovery_page)
        browser = type("FakeBrowser", (), {"contexts": [context]})()
        chatgpt_agent = agent.ChatGPTAgent("https://chatgpt.com/")
        chatgpt_agent.ensure_browser = AsyncMock(return_value=browser)
        timeout = agent.AgentError("response_timeout", "timeout")

        with (
            patch.object(agent, "wait_for_imagegen", new=AsyncMock(side_effect=[timeout, timeout])),
            patch.object(agent, "stable_wait", new=AsyncMock()),
            patch.object(agent, "imagegen_state", new=AsyncMock(return_value={"image_urls": []})),
        ):
            with self.assertRaisesRegex(agent.AgentError, "Timed out waiting"):
                await chatgpt_agent.wait_for_imagegen_with_tab_recovery(
                    original_page,
                    before_turn_count=0,
                    before_assistant_count=0,
                    conversation_url="https://chatgpt.com/c/test",
                )


if __name__ == "__main__":
    unittest.main()
