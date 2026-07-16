import asyncio
import importlib.util
import sys
import unittest
from unittest.mock import AsyncMock, patch
from pathlib import Path

MODULE_PATH = Path(__file__).with_name("chatgpt_agent.py")
SPEC = importlib.util.spec_from_file_location("chatgpt_agent", MODULE_PATH)
assert SPEC and SPEC.loader
agent = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = agent
SPEC.loader.exec_module(agent)


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
        chatgpt_agent = agent.ChatGPTAgent(
            "http://127.0.0.1:9222",
            "https://chatgpt.com/",
            mode="always_new",
        )
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
            timeout=180.0,
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
            self.assertEqual(text, "生图 draw a cat")
            events.append("type_prompt")

        with (
            patch.object(agent, "stable_wait", new=AsyncMock()),
            patch.object(agent.asyncio, "sleep", new=record_sleep),
            patch.object(agent, "turn_count", new=AsyncMock(return_value=0)),
            patch.object(agent, "assistant_messages", new=AsyncMock(return_value=[])),
            patch.object(agent, "click_element_center", new=record_click),
            patch.object(agent, "type_like_user", new=record_type),
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
        chatgpt_agent = agent.ChatGPTAgent("http://127.0.0.1:9222", "https://chatgpt.com/")
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
                timeout_seconds=180.0,
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
        chatgpt_agent = agent.ChatGPTAgent("http://127.0.0.1:9222", "https://chatgpt.com/")
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
                    timeout_seconds=180.0,
                )


if __name__ == "__main__":
    unittest.main()
