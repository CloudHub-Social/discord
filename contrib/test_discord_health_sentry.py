import contextlib
import http.client
import io
import json
import pathlib
import subprocess
import tempfile
import unittest
import urllib.error
import urllib.parse
from unittest import mock

try:
    import discord_health_sentry as adapter
except ModuleNotFoundError:
    adapter = None

ENDPOINT = "https://o123.ingest.us.sentry.io/api/456/cron/matrix-discord-health/" + "a" * 32 + "/"


class SentryTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(adapter, "Sentry adapter is not implemented")

    def test_endpoint_accepts_generated_sentry_urls(self):
        for value in (ENDPOINT, ENDPOINT.replace("/cron/", "/crons/"),
                      ENDPOINT.replace("ingest.us.", "ingest.")):
            self.assertEqual(adapter.validate_endpoint(value), value)

    def test_endpoint_rejects_untrusted_destinations_and_embedded_parameters(self):
        for value in (ENDPOINT.replace("https:", "http:"),
                      ENDPOINT.replace(".sentry.io", ".sentry.io.attacker.test"),
                      ENDPOINT.replace("o123", "user:password@o123"),
                      ENDPOINT + "?status=ok", ENDPOINT + "#fragment",
                      ENDPOINT.replace("matrix-discord-health", "another-monitor"),
                      ENDPOINT.replace("/456/", "/../")):
            with self.subTest(value=value):
                with self.assertRaises(adapter.MonitorError):
                    adapter.validate_endpoint(value)

    def test_probe_maps_actual_exit_and_attention_state(self):
        for code, state, attention, expected in (
            (0, "healthy", False, ("ok", "quiet")),
            (0, "starting", False, ("ok", "quiet")),
            (0, "recovering", False, ("ok", "quiet")),
            (1, "alert", True, ("error", "attention")),
            (2, "unknown", True, ("error", "unknown")),
        ):
            with self.subTest(code=code, state=state):
                result = subprocess.CompletedProcess([], code,
                    json.dumps({"status": state, "needs_attention": attention}), "")
                self.assertEqual(adapter.classify_probe(result), expected)

    def test_inconsistent_invalid_or_oversized_result_is_unknown(self):
        for code, output, stderr in (
            (0, '{"needs_attention":true}', ""),
            (1, '{"needs_attention":false}', ""),
            (0, '{"needs_attention":0}', ""),
            (0, '{}', ""), (3, '{"needs_attention":true}', ""),
            (0, 'not JSON', ""), (0, 'x' * 65537, ""),
            (0, '{"needs_attention":false}', "unexpected warning"),
        ):
            with self.subTest(code=code, output=output[:60]):
                result = subprocess.CompletedProcess([], code, output, stderr)
                self.assertEqual(adapter.classify_probe(result), ("error", "unknown"))

    def test_unknown_missing_or_inconsistent_status_is_rejected(self):
        for code, report in (
            (0, {"needs_attention": False}),
            (0, {"status": "unknown", "needs_attention": False}),
            (0, {"status": "unrecognized", "needs_attention": False}),
            (0, {"status": ["healthy"], "needs_attention": False}),
            (0, {"status": "alert", "needs_attention": False}),
            (1, {"status": "healthy", "needs_attention": True}),
            (1, {"status": "unknown", "needs_attention": True}),
        ):
            with self.subTest(code=code, report=report):
                result = subprocess.CompletedProcess([], code, json.dumps(report), "")
                self.assertEqual(adapter.classify_probe(result), ("error", "unknown"))

    def test_real_probe_unknown_status_never_becomes_ok(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = pathlib.Path(directory) / "probe.py"
            probe.write_text('import json; print(json.dumps({"status":"unknown","needs_attention":False}))')
            self.assertEqual(adapter.check_probe(str(probe), 2), ("error", "unknown"))

    def test_real_probe_process_gets_expected_session_argument(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = pathlib.Path(directory) / "probe.py"
            probe.write_text('import sys,json\nassert sys.argv[1:]==["--expected-sessions","2"]\nprint(json.dumps({"status":"healthy","needs_attention":False}))\n')
            self.assertEqual(adapter.check_probe(str(probe), 2), ("ok", "quiet"))

    def test_probe_timeout_and_missing_executable_are_unknown(self):
        for exc in (subprocess.TimeoutExpired("probe", 20), OSError("PRIVATE_DETAILS")):
            with mock.patch.object(adapter.subprocess, "run", side_effect=exc):
                self.assertEqual(adapter.check_probe("/does/not/matter", 2), ("error", "unknown"))

    def test_checkin_uses_same_execution_id_and_no_health_payload(self):
        response = mock.MagicMock()
        response.__enter__.return_value.status = 202
        opener = mock.Mock()
        opener.open.return_value = response
        with mock.patch.object(adapter.urllib.request, "build_opener", return_value=opener):
            adapter.send_checkin(ENDPOINT, "in_progress", "b" * 32)
            adapter.send_checkin(ENDPOINT, "error", "b" * 32)
        self.assertEqual(opener.open.call_count, 2)
        for call, status in zip(opener.open.call_args_list, ("in_progress", "error")):
            request = call.args[0]
            self.assertEqual(request.method, "GET")
            self.assertEqual(urllib.parse.parse_qs(urllib.parse.urlsplit(request.full_url).query),
                {"status": [status], "check_in_id": ["b" * 32], "environment": ["production"]})
            self.assertEqual(call.kwargs["timeout"], 5)

    def test_redirects_are_not_followed(self):
        self.assertIsNone(adapter.NoRedirect().redirect_request(None, None, 302, "move", {}, "https://elsewhere.test/"))

    def test_http_failure_is_fixed_error_without_endpoint(self):
        opener = mock.Mock()
        opener.open.side_effect = urllib.error.HTTPError(ENDPOINT, 401, "PRIVATE_DETAILS", {}, None)
        with mock.patch.object(adapter.urllib.request, "build_opener", return_value=opener):
            with self.assertRaises(adapter.MonitorError) as caught:
                adapter.send_checkin(ENDPOINT, "ok", "b" * 32)
        self.assertEqual(str(caught.exception), "sentry_delivery_failed")

    def test_protocol_errors_are_redacted(self):
        for error in (http.client.BadStatusLine("PRIVATE_DETAILS"),
                      http.client.LineTooLong("PRIVATE_DETAILS"),
                      http.client.UnknownProtocol("PRIVATE_DETAILS")):
            with self.subTest(error=type(error).__name__):
                opener = mock.Mock()
                opener.open.side_effect = error
                with mock.patch.object(adapter.urllib.request, "build_opener", return_value=opener):
                    with self.assertRaises(adapter.MonitorError) as caught:
                        adapter.send_checkin(ENDPOINT, "ok", "b" * 32)
                self.assertEqual(str(caught.exception), "sentry_delivery_failed")

    def test_protocol_failure_still_checks_real_probe_and_reports_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            probe = pathlib.Path(directory) / "probe.py"
            probe.write_text('import json; print(json.dumps({"status":"healthy","needs_attention":False}))')
            opener = mock.Mock()
            opener.open.side_effect = http.client.BadStatusLine("PRIVATE_DETAILS")
            with mock.patch.object(adapter.urllib.request, "build_opener", return_value=opener):
                report, code = adapter.run(ENDPOINT, str(probe), 2)
            self.assertEqual(code, 2)
            self.assertEqual(report["delivery"], "failed")
            self.assertFalse(report["start_accepted"])
            self.assertEqual(report["probe_outcome"], "quiet")
            self.assertNotIn("PRIVATE_DETAILS", json.dumps(report))

    def test_non_success_http_status_is_failure(self):
        response = mock.MagicMock()
        response.__enter__.return_value.status = 302
        opener = mock.Mock()
        opener.open.return_value = response
        with mock.patch.object(adapter.urllib.request, "build_opener", return_value=opener):
            with self.assertRaises(adapter.MonitorError):
                adapter.send_checkin(ENDPOINT, "ok", "b" * 32)

    def test_untrusted_status_or_execution_id_is_rejected(self):
        with self.assertRaises(adapter.MonitorError):
            adapter.send_checkin(ENDPOINT, "bogus", "b" * 32)
        with self.assertRaises(adapter.MonitorError):
            adapter.send_checkin(ENDPOINT, "ok", "unsafe?query")

    def test_failure_reports_error_and_never_invents_success(self):
        calls = []
        with mock.patch.object(adapter, "send_checkin", side_effect=lambda *args: calls.append(args)), \
             mock.patch.object(adapter, "check_probe", return_value=("error", "unknown")):
            report, code = adapter.run(ENDPOINT, "/probe", 2)
        self.assertEqual([x[1] for x in calls], ["in_progress", "error"])
        self.assertEqual(calls[0][2], calls[1][2])
        self.assertEqual(report["probe_outcome"], "unknown")
        self.assertEqual(code, 2)

    def test_delivery_failure_still_checks_probe_and_is_not_success(self):
        with mock.patch.object(adapter, "send_checkin", side_effect=adapter.MonitorError("sentry_delivery_failed")), \
             mock.patch.object(adapter, "check_probe", return_value=("ok", "quiet")) as probe:
            report, code = adapter.run(ENDPOINT, "/probe", 2)
        probe.assert_called_once_with("/probe", 2)
        self.assertEqual(report["delivery"], "failed")
        self.assertEqual(code, 2)

    def test_cli_reads_credential_file_without_printing_it(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "sentry-cron-url"
            path.write_text(ENDPOINT + "\n")
            with mock.patch.object(adapter, "run", return_value=({"delivery": "accepted", "probe_outcome": "quiet"}, 0)) as run:
                output = io.StringIO()
                with contextlib.redirect_stdout(output):
                    code = adapter.main(["--url-file", str(path)])
                self.assertEqual(code, 0)
                self.assertEqual(run.call_args.args[0], ENDPOINT)
                self.assertNotIn("a" * 32, output.getvalue())

    def test_missing_credential_fails_closed(self):
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            code = adapter.main(["--url-file", "/not-present/sentry-cron-url"])
        self.assertEqual(code, 2)
        self.assertEqual(json.loads(output.getvalue())["reason"], "sentry_configuration_unavailable")


if __name__ == "__main__":
    unittest.main()
