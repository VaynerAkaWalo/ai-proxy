import http.server
import json
import os
from pathlib import Path
import subprocess
import threading
import unittest


ROOT = Path(__file__).resolve().parents[1]


class DashboardApplyTest(unittest.TestCase):
    def apply(self, status=200, token="dashboard-owner-token"):
        requests = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_PUT(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                requests.append((self.path, self.headers, json.loads(body)))
                self.send_response(status)
                self.send_header("Location", "/redirected")
                self.end_headers()
                self.wfile.write(b'{"result":"test response"}')

            def log_message(self, *args):
                pass

        with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
            thread = threading.Thread(target=server.serve_forever)
            thread.start()

            try:
                result = subprocess.run(
                    ["bash", str(ROOT / "deploy/apply-analytics-dashboard.sh")],
                    cwd="/tmp",
                    env={
                        **os.environ,
                        "ANALYTICS_URL": f"http://127.0.0.1:{server.server_port}/",
                        "ANALYTICS_DASHBOARD_TOKEN": token,
                    },
                    capture_output=True,
                    text=True,
                    timeout=10,
                )
            finally:
                server.shutdown()
                thread.join()

        return result, requests

    def test_applies_versioned_layout_for_create_and_update(self):
        definition = json.loads((ROOT / "dashboards/llm-usage.json").read_text())

        for status in (200, 201):
            with self.subTest(status=status):
                result, requests = self.apply(status)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(requests), 1)
                path, headers, body = requests[0]
                self.assertEqual(path, "/api/dashboards/llm-usage")
                self.assertEqual(headers["Authorization"], "Bearer dashboard-owner-token")
                self.assertEqual(headers["Content-Type"], "application/json")
                self.assertEqual(body, definition)
                self.assertNotIn("visibility", body)
                self.assertNotIn("editable", body)

    def test_rejects_api_errors_and_redirects_without_retrying(self):
        for status in (302, 401, 403, 422, 500):
            with self.subTest(status=status):
                result, requests = self.apply(status)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(len(requests), 1)
                self.assertNotIn("dashboard-owner-token", result.stdout + result.stderr)

    def test_requires_explicit_dashboard_credentials_before_sending(self):
        result, requests = self.apply(token="")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ANALYTICS_DASHBOARD_TOKEN", result.stderr)
        self.assertEqual(requests, [])


if __name__ == "__main__":
    unittest.main()
