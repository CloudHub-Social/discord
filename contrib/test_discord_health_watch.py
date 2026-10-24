import concurrent.futures
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch
from discord_health_watch import checkpoint, run_check


def report(reason=None):
    return {'status': 'alert' if reason else 'healthy', 'needs_attention': bool(reason),
            'findings': [{'session': 'user', 'reason': reason, 'unknown': False}] if reason else []}


class WatchTests(unittest.TestCase):
    def test_first_problem_notifies_and_repeats_are_quiet(self):
        with tempfile.TemporaryDirectory() as folder:
            state = pathlib.Path(folder)
            self.assertTrue(checkpoint(report('heartbeat_stale'), state)['notify'])
            self.assertFalse(checkpoint(report('heartbeat_stale'), state)['notify'])

    def test_healthy_reset_allows_recurrence(self):
        with tempfile.TemporaryDirectory() as folder:
            state = pathlib.Path(folder)
            checkpoint(report('heartbeat_stale'), state)
            self.assertFalse(checkpoint(report(), state)['notify'])
            self.assertTrue(checkpoint(report('heartbeat_stale'), state)['notify'])

    def test_changed_problem_notifies(self):
        with tempfile.TemporaryDirectory() as folder:
            state = pathlib.Path(folder)
            checkpoint(report('heartbeat_stale'), state)
            self.assertTrue(checkpoint(report('invalid_credentials'), state)['notify'])

    def test_age_counters_do_not_retrigger(self):
        with tempfile.TemporaryDirectory() as folder:
            state = pathlib.Path(folder)
            checkpoint({**report('heartbeat_stale'), 'age': 301}, state)
            self.assertFalse(checkpoint({**report('heartbeat_stale'), 'age': 400}, state)['notify'])

    def test_concurrent_checks_do_not_duplicate(self):
        with tempfile.TemporaryDirectory() as folder:
            state = pathlib.Path(folder)
            with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                results = list(pool.map(lambda _: checkpoint(report('heartbeat_stale'), state), range(8)))
            self.assertEqual(sum(r['notify'] for r in results), 1)
            self.assertEqual((state / 'state.json').stat().st_mode & 0o777, 0o600)

    def test_ssh_failure_is_unknown_and_redacted(self):
        proc = subprocess.CompletedProcess([], 255, '', 'PRIVATE_TEST_SECRET')
        with patch('discord_health_watch.subprocess.run', return_value=proc):
            result = run_check('example.test', 2)
        self.assertEqual(result['status'], 'unknown')
        self.assertTrue(result['needs_attention'])
        self.assertNotIn('PRIVATE_TEST_SECRET', json.dumps(result))

    def test_timeout_is_unknown(self):
        with patch('discord_health_watch.subprocess.run', side_effect=subprocess.TimeoutExpired('ssh', 90)):
            self.assertEqual(run_check('example.test', 2)['status'], 'unknown')

    def test_invalid_probe_json_is_not_healthy(self):
        proc = subprocess.CompletedProcess([], 0, '{"status":"healthy"}', '')
        with patch('discord_health_watch.subprocess.run', return_value=proc):
            self.assertEqual(run_check('example.test', 2)['status'], 'unknown')

    def test_probe_failure_exit_preserves_actual_finding(self):
        proc = subprocess.CompletedProcess([], 1, json.dumps(report('service_down')), '')
        with patch('discord_health_watch.subprocess.run', return_value=proc):
            self.assertEqual(run_check('example.test', 2)['findings'][0]['reason'], 'service_down')


if __name__ == '__main__':
    unittest.main()
