import importlib.util
import sys
import unittest
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
            "has_reply_actions": True,
            "has_preview": False,
            "is_streaming": False,
            "has_stop_button": False,
        }

    def test_accepts_completed_image_turn(self):
        self.assertTrue(agent.imagegen_state_ready(self.complete_state()))

    def test_rejects_preview_phase(self):
        state = self.complete_state()
        state["has_preview"] = True
        self.assertFalse(agent.imagegen_state_ready(state))

    def test_rejects_streaming_or_stop_button(self):
        for key in ("is_streaming", "has_stop_button"):
            with self.subTest(key=key):
                state = self.complete_state()
                state[key] = True
                self.assertFalse(agent.imagegen_state_ready(state))

    def test_requires_reply_actions_and_loaded_images(self):
        for key in ("has_images", "images_loaded", "has_reply_actions"):
            with self.subTest(key=key):
                state = self.complete_state()
                state[key] = False
                self.assertFalse(agent.imagegen_state_ready(state))


if __name__ == "__main__":
    unittest.main()
