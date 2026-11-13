"""Small stdlib regressions for Safari smoke failure reporting."""
import contextlib
import io
import json
import unittest
from unittest.mock import Mock, patch
from urllib.error import HTTPError

import safari_smoke


class SmokeReportingTests(unittest.TestCase):
    def test_failure_retains_progress_and_cleanup_does_not_mask_it(self):
        driver = Mock(session=None)

        def request(method, _path, _body=None):
            if method == "POST":
                return {"sessionId": "owned", "capabilities": {"browserName": "safari"}}
            raise RuntimeError("session cleanup failed")

        def wait(script):
            if "writes" in script:
                raise AssertionError("primary diff failure")
            return True

        driver.request.side_effect = request
        driver.wait.side_effect = wait
        driver.js.side_effect = lambda script: (
            "Safari fixture UA" if "userAgent" in script else {"requests": 1}
        )
        result = safari_smoke.run(driver, "http://127.0.0.1/", False)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(result["checks"], ["trusted direct clipboard positive control"])
        self.assertEqual(result["userAgent"], "Safari fixture UA")
        self.assertIn("primary diff failure", result["error"])
        self.assertEqual(result["audit"], {"requests": 1})
        self.assertEqual(result["cleanupErrors"], ["session cleanup failed"])

    def test_session_creation_failure_does_not_delete_an_unowned_session(self):
        driver = Mock(session=None)
        driver.request.side_effect = RuntimeError("session creation failed")
        result = safari_smoke.run(driver, "http://127.0.0.1/", False)
        self.assertIn("session creation failed", result["error"])
        self.assertEqual(driver.request.call_count, 1)

    def test_http_error_includes_webdriver_response(self):
        error = HTTPError("http://driver/", 500, "error", {},
                          io.BytesIO(b'{"value":{"error":"session not created"}}'))
        with patch("urllib.request.urlopen", side_effect=error):
            with self.assertRaisesRegex(RuntimeError, "HTTP 500.*session not created"):
                safari_smoke.Driver("http://driver").request("POST", "/session", {})

    def test_main_emits_failed_result_and_nonzero_exit(self):
        result = {"status": "failed", "checks": ["completed"], "error": "primary"}
        output = io.StringIO()
        with patch("sys.argv", ["safari_smoke.py", "--fixture-url", "http://fixture/"]), \
                patch.object(safari_smoke, "run", return_value=result), \
                contextlib.redirect_stdout(output):
            self.assertEqual(safari_smoke.main(), 1)
        self.assertEqual(json.loads(output.getvalue()), result)


if __name__ == "__main__":
    unittest.main()
