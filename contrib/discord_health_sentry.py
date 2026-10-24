#!/usr/bin/env python3
"""Send private, status-only Sentry Cron check-ins for the existing host probe.

No gateway payloads, account identifiers, credentials, or exception text leave
this adapter. Installing it does not create a Sentry monitor or an alert rule.
"""
import argparse
import json
import os
import pathlib
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid


class MonitorError(Exception):
    """Fixed error codes only; never include endpoints or external error text."""


def validate_endpoint(value):
    try:
        url = urllib.parse.urlsplit(value)
        valid = (
            url.scheme == "https" and url.netloc == url.hostname
            and re.fullmatch(r"o[0-9]+\.ingest(?:\.[a-z0-9-]+)?\.sentry\.io", url.hostname or "")
            and re.fullmatch(r"/api/[0-9]+/crons?/matrix-discord-health/[0-9a-f]{32}/", url.path)
            and not url.query and not url.fragment
        )
    except ValueError:
        valid = False
    if not valid:
        raise MonitorError("sentry_endpoint_invalid")
    return value


def classify_probe(result):
    if len(result.stdout) > 65536 or result.stderr:
        return "error", "unknown"
    try:
        report = json.loads(result.stdout)
    except (TypeError, ValueError):
        return "error", "unknown"
    if not isinstance(report, dict) or type(report.get("needs_attention")) is not bool:
        return "error", "unknown"
    state = report.get("status")
    if (result.returncode == 0 and report["needs_attention"] is False
            and state in ("healthy", "starting", "recovering")):
        return "ok", "quiet"
    if result.returncode == 1 and report["needs_attention"] is True and state == "alert":
        return "error", "attention"
    return "error", "unknown"


def check_probe(path, expected):
    try:
        result = subprocess.run(
            [sys.executable, path, "--expected-sessions", str(expected)],
            capture_output=True, text=True, timeout=20,
        )
    except (OSError, subprocess.TimeoutExpired, UnicodeError):
        return "error", "unknown"
    return classify_probe(result)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # The endpoint contains an ingestion credential: never forward it.
        return None


def send_checkin(endpoint, status, check_in_id):
    validate_endpoint(endpoint)
    if status not in ("in_progress", "ok", "error") or not re.fullmatch(r"[0-9a-f]{32}", check_in_id):
        raise MonitorError("sentry_checkin_invalid")
    query = urllib.parse.urlencode({
        "status": status, "check_in_id": check_in_id, "environment": "production",
    })
    request = urllib.request.Request(endpoint + "?" + query, method="GET",
                                     headers={"User-Agent": "matrix-discord-health/1"})
    try:
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=5) as response:
            if not 200 <= response.status < 300:
                raise MonitorError("sentry_delivery_failed")
    except (OSError, urllib.error.URLError, ValueError):
        raise MonitorError("sentry_delivery_failed") from None


def run(endpoint, probe, expected):
    check_in_id = uuid.uuid4().hex
    started = True
    try:
        send_checkin(endpoint, "in_progress", check_in_id)
    except MonitorError:
        started = False
    status, outcome = check_probe(probe, expected)
    result = {"check_in_id": check_in_id, "start_accepted": started,
              "probe_outcome": outcome, "check_in_status": status}
    try:
        send_checkin(endpoint, status, check_in_id)
    except MonitorError:
        return {**result, "delivery": "failed"}, 2
    return {**result, "delivery": "accepted"}, {"quiet": 0, "attention": 1, "unknown": 2}[outcome]


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    credentials = os.environ.get("CREDENTIALS_DIRECTORY", "/etc/matrix-discord-health")
    parser.add_argument("--url-file", default=str(pathlib.Path(credentials) / "sentry-cron-url"))
    parser.add_argument("--probe", default="/usr/local/libexec/matrix-discord-health.py")
    parser.add_argument("--expected-sessions", type=int, choices=range(1, 101), default=2)
    args = parser.parse_args(argv)
    try:
        with open(args.url_file, encoding="utf-8") as source:
            endpoint = source.read(4097).strip()
        if len(endpoint) > 4096:
            raise MonitorError("sentry_endpoint_invalid")
        validate_endpoint(endpoint)
    except (OSError, UnicodeError):
        print(json.dumps({"delivery": "not_attempted", "reason": "sentry_configuration_unavailable"}))
        return 2
    except MonitorError as exc:
        print(json.dumps({"delivery": "not_attempted", "reason": str(exc)}))
        return 2
    result, code = run(endpoint, args.probe, args.expected_sessions)
    print(json.dumps(result, sort_keys=True))
    return code


if __name__ == "__main__":
    sys.exit(main())
