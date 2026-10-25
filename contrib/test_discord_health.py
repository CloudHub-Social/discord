import hashlib
import json
import sys
import tempfile
import pathlib
from unittest.mock import patch
import unittest

from discord_health import ProbeError, assess, command, notification_key, parse_record, presence_snapshot, journal_records, collect

INV = 'a' * 32
NOW = 10000.0


def row(body, user='@user:example.test', stamp=9990, component=None, binary=False):
    fields = {'level': 'info', 'message': body, 'user_id': user}
    if component:
        fields['component'] = component
    message = json.dumps(fields)
    if binary:
        message = list(message.encode())
    return {'_SYSTEMD_UNIT': 'matrix-mautrix-discord.service',
            '_SYSTEMD_INVOCATION_ID': INV, '__REALTIME_TIMESTAMP': str(int(stamp * 1e6)),
            'MESSAGE': message}


def event(kind, when, session='user'):
    return {'kind': kind, 'timestamp': when, 'session': session}


def healthy():
    return [event('ready', 1000), event('ready', 1000, 'bot'),
            event('ack', 9990), event('ack', 9990, 'bot')]


class ParseTests(unittest.TestCase):
    def test_heartbeat_is_identity_scoped_and_redacted(self):
        result = parse_record(row('gateway_health heartbeat_ack=true', component='discordgo'), INV)
        self.assertEqual(result['kind'], 'ack')
        self.assertEqual(result['timestamp'], 9990)
        self.assertEqual(result['session'], hashlib.sha256(b'@user:example.test').hexdigest()[:12])
        self.assertNotIn('@user', json.dumps(result))

    def test_byte_array_journal_message(self):
        self.assertEqual(parse_record(row('Discord connection ready', binary=True), INV)['kind'], 'ready')

    def test_colored_console_message(self):
        data = row('unused')
        data['MESSAGE'] = '\x1b[90m12:00:00\x1b[0m INF gateway_health heartbeat_ack=true component=discordgo user_id=@user:example.test'
        self.assertEqual(parse_record(data, INV)['kind'], 'ack')

    def test_diagnostics_without_sensitive_close_text(self):
        result = parse_record(row('gateway_diagnostic kind=read_error close_code=4002 error_class=websocket_close', component='discordgo'), INV)
        self.assertEqual(result['cause'], 'read_error')
        self.assertEqual(result['close_code'], 4002)

    def test_wrong_invocation_is_not_healthy(self):
        with self.assertRaises(ProbeError):
            parse_record(row('Discord connection ready'), 'b' * 32)

    def test_unrelated_payload_does_not_count_as_heartbeat(self):
        self.assertIsNone(parse_record(row('Received message: gateway_health heartbeat_ack=true SECRET'), INV))
        self.assertIsNone(parse_record(row('gateway_health heartbeat_ack=true', component='untrusted'), INV))

    def test_missing_identity_is_not_fabricated(self):
        value = row('Discord connection ready')
        value['MESSAGE'] = json.dumps({'level': 'info', 'message': 'Discord connection ready'})
        with self.assertRaises(ProbeError):
            parse_record(value, INV)

    def test_console_caller_with_function_heartbeat(self):
        data = row('unused')
        data['MESSAGE'] = '2026-10-24T15:45:23Z INF wsapi.go:874:onEvent() > gateway_health heartbeat_ack=true component=discordgo user_id=@user:example.test'
        result = parse_record(data, INV)
        self.assertIsNotNone(result)
        self.assertEqual(result['kind'], 'ack')

    def test_console_caller_with_function_diagnostic(self):
        data = row('unused')
        data['MESSAGE'] = '2026-10-24T15:45:23Z WRN gateway_diagnostics.go:39:logGatewayDiagnostic() > gateway_diagnostic kind=read_error close_code=4002 error_class=websocket_close component=discordgo user_id=@user:example.test'
        result = parse_record(data, INV)
        self.assertIsNotNone(result)
        self.assertEqual(result['cause'], 'read_error')
        self.assertEqual(result['close_code'], 4002)


class AssessTests(unittest.TestCase):
    def report(self, events, started=500, expected=2):
        return assess(events, NOW, started, expected)

    def test_recent_heartbeat_quiet(self):
        result = self.report(healthy())
        self.assertEqual(result['status'], 'healthy')
        self.assertFalse(result['needs_attention'])

    def test_brief_recovered_disconnect_quiet(self):
        result = self.report(healthy() + [event('disconnect', 9900), event('ready', 9904)])
        self.assertEqual(result['status'], 'healthy')
        self.assertFalse(result['needs_attention'])

    def test_short_current_recovery_quiet(self):
        result = self.report(healthy() + [event('disconnect', 9995)])
        self.assertEqual(result['status'], 'recovering')
        self.assertFalse(result['needs_attention'])

    def test_recovery_stalled_over_five_minutes(self):
        result = self.report(healthy() + [event('disconnect', 9600), event('connecting', 9990)])
        self.assertIn('recovery_stalled', [f['reason'] for f in result['findings']])

    def test_brief_ready_ack_does_not_reset_failed_episode(self):
        entries = [event('ready', 1000), event('ack', 1100),
                   event('disconnect', 9000), event('connecting', 9790),
                   event('ready', 9791), event('ack', 9792), event('disconnect', 9800)]
        result = self.report(entries, expected=1)
        self.assertTrue(result['needs_attention'])
        self.assertIn('recovery_stalled', [f['reason'] for f in result['findings']])
        self.assertEqual(result['sessions']['user']['outage_age_seconds'], 1000)

    def test_pending_ready_keeps_failure_until_sustained_ack(self):
        entries = [event('disconnect', 9000), event('ready', 9980), event('ack', 9990)]
        self.assertTrue(self.report(entries, expected=1)['needs_attention'])

    def test_stable_recovery_requires_ack_after_stability_window(self):
        entries = [event('disconnect', 9000), event('ready', 9900), event('ack', 9901)]
        self.assertTrue(self.report(entries, expected=1)['needs_attention'])
        self.assertFalse(self.report(entries + [event('ack', 9930)], expected=1)['needs_attention'])

    def test_failure_after_stable_recovery_gets_new_grace(self):
        entries = [event('disconnect', 9000), event('ready', 9900),
                   event('ack', 9930), event('disconnect', 9995)]
        result = self.report(entries, expected=1)
        self.assertEqual(result['status'], 'recovering')
        self.assertEqual(result['sessions']['user']['outage_age_seconds'], 5)

    def test_manual_disconnect_resets_failure_episode(self):
        entries = [event('disconnect', 9000), event('manual', 9500),
                   event('connecting', 9990), event('ack', 9991)]
        result = self.report(entries, expected=1)
        self.assertEqual(result['status'], 'recovering')
        self.assertEqual(result['sessions']['user']['outage_age_seconds'], 10)

    def test_brief_ready_does_not_clear_invalid_credentials(self):
        entries = [event('invalid_auth', 9991), event('ready', 9992), event('ack', 9993)]
        self.assertIn('invalid_credentials', [f['reason'] for f in self.report(entries, expected=1)['findings']])

    def test_brief_ack_does_not_clear_prior_stale_heartbeat(self):
        entries = [event('ready', 1000), event('ack', 9000), event('connecting', 9990),
                   event('ready', 9991), event('ack', 9992)]
        self.assertIn('heartbeat_stale', [f['reason'] for f in self.report(entries, expected=1)['findings']])

    def test_late_ack_on_old_ready_requires_sustained_heartbeat_recovery(self):
        entries = [event('ready', 1000), event('ack', 9000), event('ack', 9960)]
        self.assertTrue(self.report(entries, expected=1)['needs_attention'])
        self.assertFalse(self.report(entries + [event('ack', 9990)], expected=1)['needs_attention'])

    def test_stale_heartbeat_not_hidden_by_old_ready(self):
        result = self.report([event('ready', 1000), event('ack', 9600)], expected=1)
        self.assertIn('heartbeat_stale', [f['reason'] for f in result['findings']])

    def test_no_evidence_is_unknown_not_healthy(self):
        result = self.report([])
        self.assertEqual(result['status'], 'unknown')
        self.assertTrue(result['needs_attention'])

    def test_startup_has_five_minute_grace(self):
        result = self.report([], started=9950)
        self.assertEqual(result['status'], 'starting')
        self.assertFalse(result['needs_attention'])

    def test_missing_second_account_is_unknown(self):
        result = self.report([event('ready', 9000), event('ack', 9990)])
        self.assertIn('session_coverage_missing', [f['reason'] for f in result['findings']])

    def test_six_disconnects_in_fifteen_minutes_alert(self):
        entries = healthy() + [event('disconnect', 9200 + i * 100) for i in range(6)] + [event('ready', 9800)]
        result = self.report(entries)
        self.assertIn('reconnect_storm', [f['reason'] for f in result['findings']])

    def test_storms_are_per_account_not_combined(self):
        entries = healthy()
        for session in ('user', 'bot'):
            entries += [event('disconnect', 9200 + i * 100, session) for i in range(3)]
            entries += [event('ready', 9800, session)]
        self.assertFalse(self.report(entries)['needs_attention'])

    def test_old_disconnects_do_not_create_storm(self):
        entries = healthy() + [event('disconnect', 8000 + i * 100) for i in range(7)] + [event('ready', 9000)]
        self.assertFalse(self.report(entries)['needs_attention'])

    def test_invalid_credentials_alert_immediately(self):
        result = self.report(healthy() + [event('invalid_auth', 9999)])
        self.assertIn('invalid_credentials', [f['reason'] for f in result['findings']])

    def test_new_ready_clears_old_auth_failure(self):
        self.assertFalse(self.report(healthy() + [event('invalid_auth', 9400), event('ready', 9500)])['needs_attention'])

    def test_intentional_disconnect_quiet(self):
        result = self.report([event('manual', 8000), event('ack', 9990, 'bot')])
        self.assertFalse(result['needs_attention'])
        self.assertEqual(result['sessions']['user']['status'], 'intentionally_disconnected')

    def test_new_connect_after_manual_disconnect_is_monitored(self):
        result = self.report([event('manual', 8000), event('connecting', 9000)], expected=1)
        self.assertTrue(result['needs_attention'])

    def test_future_clock_data_rejected(self):
        with self.assertRaises(ProbeError):
            self.report([event('ack', 10100)])

    def test_old_container_heartbeat_not_reused(self):
        result = self.report(healthy(), started=9995)
        self.assertEqual(result['status'], 'starting')
        self.assertEqual(result['sessions'], {})

    def test_alert_signature_ignores_changing_age(self):
        a = self.report([event('ack', 9000)], expected=1)
        b = self.report([event('ack', 8900)], expected=1)
        self.assertEqual(notification_key(a), notification_key(b))
        self.assertEqual(notification_key(self.report(healthy())), '')


class CollectorTests(unittest.TestCase):
    def test_duplicate_configuration_is_unknown(self):
        with tempfile.TemporaryDirectory() as folder:
            p = pathlib.Path(folder) / 'config.yaml'
            p.write_text('sync_matrix_presence_to_discord: false\nsync_matrix_presence_to_discord: invalid\nsync_matrix_status_to_discord: false\n')
            with patch('discord_health.CONFIG', p), self.assertRaises(ProbeError):
                presence_snapshot()

    def test_recent_window_truncation_is_unknown(self):
        with patch('discord_health.command', return_value='{}\n' * 2001), self.assertRaises(ProbeError):
            journal_records(INV, True)

    def test_stopped_service_is_not_healthy(self):
        with patch('discord_health.unit_snapshot', return_value={'ActiveState': 'inactive', 'SubState': 'dead'}):
            result = collect()
        self.assertEqual(result['status'], 'alert')
        self.assertEqual(result['findings'][0]['reason'], 'service_down')

    def test_deployment_change_invalidates_snapshot(self):
        before = {'ActiveState': 'active', 'SubState': 'running', 'InvocationID': INV, 'NRestarts': '0'}
        after = {**before, 'InvocationID': 'b' * 32}
        container = {'running': True}
        with patch('discord_health.unit_snapshot', side_effect=[before, after]), \
             patch('discord_health.container_snapshot', return_value=container), \
             patch('discord_health.presence_snapshot', return_value={'flags': {}}), \
             patch('discord_health.journal_records', return_value=[]), self.assertRaises(ProbeError):
            collect()


class CommandTests(unittest.TestCase):
    def test_output_is_bounded(self):
        with self.assertRaises(ProbeError):
            command([sys.executable, '-c', 'print("x" * 1000)'], 'test', limit=100)

    def test_subprocess_failure_is_explicit(self):
        with self.assertRaises(ProbeError):
            command([sys.executable, '-c', 'raise SystemExit(3)'], 'test')

    def test_subprocess_timeout_is_explicit(self):
        with self.assertRaises(ProbeError):
            command([sys.executable, '-c', 'import time; time.sleep(2)'], 'test', timeout=0.05)


if __name__ == '__main__':
    unittest.main()
