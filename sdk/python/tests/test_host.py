import threading
import time
import unittest

from agentos_runtime.host import RuntimeHost, _Execution


def start_request(execution_id: str) -> dict:
    return {
        "executionId": execution_id,
        "agentVersionRef": "fixture@1.0.0",
        "goal": "test",
        "input": {},
        "capabilities": {"tools": [], "models": [], "memory": [], "secrets": []},
    }


class BlockingRuntime:
    def run(self, _request, _emit, stop_event):
        stop_event.wait(1)
        return {"late": True}

    def checkpoint(self, _execution_id):
        return {"schemaVersion": "fixture/v1", "state": {}, "createdAt": "now"}

    def restore(self, _execution_id, _checkpoint):
        return None


class FailingRuntime(BlockingRuntime):
    def run(self, _request, _emit, _stop_event):
        raise RuntimeError("secret-provider-token")


class StubbornRuntime(BlockingRuntime):
    def run(self, _request, _emit, _stop_event):
        time.sleep(0.1)
        return {"late": True}


class RuntimeHostTests(unittest.TestCase):
    def run_execution(self, host: RuntimeHost, execution_id: str) -> _Execution:
        execution = _Execution(digest=b"digest")
        with host.lock:
            host.executions[execution_id] = execution
            host.active += 1
        worker = threading.Thread(
            target=host._run,
            args=(start_request(execution_id), execution),
            daemon=True,
        )
        worker.start()
        worker.join(1)
        self.assertFalse(worker.is_alive())
        return execution

    def test_execution_watchdog_is_terminal_and_capacity_safe(self):
        host = RuntimeHost(BlockingRuntime(), "fixture", execution_timeout=0.02)
        execution = self.run_execution(host, "timeout")
        self.assertEqual(execution.result["status"], "FAILED")
        self.assertEqual(execution.result["errorCode"], "EXECUTION_TIMEOUT")
        self.assertEqual(host.active, 0)

    def test_adapter_exception_is_not_disclosed(self):
        host = RuntimeHost(FailingRuntime(), "fixture")
        execution = self.run_execution(host, "failure")
        self.assertEqual(execution.result["errorCode"], "ADAPTER_FAILED")
        self.assertNotIn("secret-provider-token", execution.result["error"])

    def test_stuck_execution_escalates_to_force_terminator(self):
        called = threading.Event()
        host = RuntimeHost(
            StubbornRuntime(), "isolated", execution_timeout=0.01,
            termination_grace=0.01, force_terminate=lambda _execution_id: called.set(),
        )
        execution = _Execution(digest=b"digest")
        with host.lock:
            host.executions["stuck"] = execution
            host.active += 1
        worker = threading.Thread(target=host._run, args=(start_request("stuck"), execution), daemon=True)
        worker.start()
        self.assertTrue(called.wait(0.2))
        worker.join(0.3)

    def test_control_operation_has_deadline(self):
        host = RuntimeHost(BlockingRuntime(), "fixture", control_timeout=0.01)
        with self.assertRaises(TimeoutError):
            host._call_with_timeout(time.sleep, 0.2)

    def test_forward_capability_fields_are_accepted(self):
        body = start_request("future")
        body["futureField"] = {"ignored": True}
        body["capabilities"]["memorySensitivities"] = ["confidential"]
        RuntimeHost._validate_start(body)


if __name__ == "__main__":
    unittest.main()
