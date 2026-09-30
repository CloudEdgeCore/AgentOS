"""Tests for framework ecosystem wrappers."""

import threading
import unittest

from agentos_runtime.ecosystem import FrameworkAdapterRuntime


class EcosystemWrapperTests(unittest.TestCase):
    def test_framework_adapter_runtime_execution(self):
        called = False

        def mock_runner(req, client, stop_event):
            nonlocal called
            called = True
            return {"greeting": "hello " + req.get("name", "world")}

        runtime = FrameworkAdapterRuntime("mock_framework", mock_runner)

        events = []
        def emit(event, data):
            events.append((event, data))

        stop_event = threading.Event()
        result = runtime.run({"executionId": "test-exec", "name": "agent"}, emit, stop_event)

        self.assertTrue(called)
        self.assertEqual(result.get("greeting"), "hello agent")
        self.assertTrue(any(e[0] == "mock_framework.starting" for e in events))
        self.assertTrue(any(e[0] == "mock_framework.completed" for e in events))


if __name__ == "__main__":
    unittest.main()
