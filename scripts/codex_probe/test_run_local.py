import os
from pathlib import Path
import signal
import subprocess
import sys
import unittest
from unittest import mock
import copy

import run_local


class ModelSelectionTests(unittest.TestCase):
    def test_cli_defaults_follow_discovered_default(self):
        self.assertTrue(hasattr(run_local, 'parse_args'), 'runner needs testable CLI defaults')
        args = run_local.parse_args(['--binary', '/tmp/artex'])
        self.assertEqual(run_local.select_models(
            {'data': [{'id': 'first'}, {'id': 'account-default', 'default': True}]},
            args.planner_model, args.worker_model),
            {'planner': 'account-default', 'worker': 'account-default'})

    def test_first_available_is_fallback_and_explicit_roles_stay_distinct(self):
        self.assertTrue(hasattr(run_local, 'select_models'))
        available = {'data': [{'id': 'first'}, {'id': 'second'}]}
        self.assertEqual(run_local.select_models(available), {'planner': 'first', 'worker': 'first'})
        self.assertEqual(run_local.select_models(available, 'second', 'first'),
                         {'planner': 'second', 'worker': 'first'})

    def test_unavailable_role_reports_available_ids(self):
        self.assertTrue(hasattr(run_local, 'select_models'))
        for role in ('planner', 'worker'):
            with self.subTest(role=role), self.assertRaisesRegex(
                    ValueError, role + '.*unavailable.*missing.*available.*first'):
                run_local.select_models({'data': [{'id': 'first'}]}, **{role: 'missing'})

    def test_empty_or_malformed_discovery_is_rejected(self):
        self.assertTrue(hasattr(run_local, 'select_models'))
        for response in ({'data': []}, {}, {'data': [{'id': ''}]}, {'data': None}):
            with self.subTest(response=response), self.assertRaisesRegex(ValueError, 'models'):
                run_local.select_models(response)

    def test_discovery_uses_authenticated_bridge_endpoint(self):
        self.assertTrue(hasattr(run_local, 'discover_models'))
        # HTTP is the external boundary; validate the actual request and decoding.
        def reply(request, timeout):
            self.assertEqual(request.full_url, 'http://127.0.0.1:1234/v1/models')
            self.assertEqual(request.get_header('Authorization'), 'Bearer local-secret')
            response = mock.MagicMock()
            response.__enter__.return_value.read.return_value = b'{"data":[{"id":"available"}]}'
            return response
        with mock.patch('run_local.urllib.request.urlopen', side_effect=reply):
            self.assertEqual(run_local.discover_models(1234, 'local-secret'),
                             {'data': [{'id': 'available'}]})


class PricingChecksTests(unittest.TestCase):
    def test_reported_missing_attempt_is_valid_only_as_unpriced_partial_coverage(self):
        models, rows = self.fixture()
        rows.append({'model': 'priced', 'usage_known': False, 'cost_snapshot': None})
        models[0].update(calls=2, unpriced_calls=1, usage_missing_calls=1)
        self.assertTrue(run_local.model_costs_consistent(models, rows))
        models[0]['usage_missing_calls'] = 0
        self.assertFalse(run_local.model_costs_consistent(models, rows))
        models[0]['usage_missing_calls'] = 1
        rows[-1]['cost_snapshot'] = copy.deepcopy(rows[0]['cost_snapshot'])
        self.assertFalse(run_local.model_costs_consistent(models, rows))

    def fixture(self):
        cost = {'currency': 'USD', 'basis': 'standard_api_equivalent',
                'price_version': 'test-version', 'source_url': 'https://example.com/pricing',
                'input_usd': .1, 'cache_read_usd': .01, 'cache_write_usd': 0,
                'output_usd': .2, 'total_usd': .31, 'without_cache_usd': .4,
                'cache_read_savings_usd': .09, 'cache_write_premium_usd': 0,
                'net_savings_usd': .09}
        rows = [{'model': 'priced', 'usage_known': True, 'cost_snapshot': cost},
                {'model': 'new-account-model', 'usage_known': True, 'cost_snapshot': None}]
        models = [{'model': 'priced', 'calls': 1, 'priced_calls': 1, 'unpriced_calls': 0,
                   'usage_missing_calls': 0, 'cost': copy.deepcopy(cost)},
                  {'model': 'new-account-model', 'calls': 1, 'priced_calls': 0,
                   'unpriced_calls': 1, 'usage_missing_calls': 0, 'cost': None}]
        return models, rows

    def test_unknown_pricing_is_accepted_only_with_explicit_unpriced_coverage(self):
        self.assertTrue(hasattr(run_local, 'model_costs_consistent'))
        models, rows = self.fixture()
        self.assertTrue(run_local.model_costs_consistent(models, rows))
        self.assertTrue(run_local.model_costs_consistent(models[1:], rows[1:]))
        models[1]['unpriced_calls'] = 0
        self.assertFalse(run_local.model_costs_consistent(models, rows))

    def test_priced_costs_must_match_persisted_snapshots(self):
        self.assertTrue(hasattr(run_local, 'model_costs_consistent'))
        for field, value in [('cost', None), ('priced_calls', 0), ('usage_missing_calls', 1)]:
            models, rows = self.fixture()
            models[0][field] = value
            with self.subTest(field=field):
                self.assertFalse(run_local.model_costs_consistent(models, rows))
        models, rows = self.fixture()
        models[0]['cost']['total_usd'] = 99
        self.assertFalse(run_local.model_costs_consistent(models, rows))

    def test_missing_usage_or_models_cannot_masquerade_as_unknown_pricing(self):
        self.assertTrue(hasattr(run_local, 'model_costs_consistent'))
        models, rows = self.fixture()
        self.assertFalse(run_local.model_costs_consistent([], []))
        self.assertFalse(run_local.model_costs_consistent(models[:1], rows))
        rows[1]['usage_known'] = False
        self.assertFalse(run_local.model_costs_consistent(models, rows))


class CleanupTests(unittest.TestCase):
    def test_sigterm_runs_finally(self):
        code = """
import time
import run_local
run_local.install_shutdown_handlers()
try:
    print('ready', flush=True)
    time.sleep(30)
finally:
    print('cleaned', flush=True)
"""
        process = subprocess.Popen([sys.executable, '-c', code], cwd=Path(__file__).parent,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            self.assertEqual(process.stdout.readline().strip(), 'ready')
            process.terminate()
            out, err = process.communicate(timeout=3)
            self.assertIn('cleaned', out, err)
            self.assertEqual(process.returncode, 128 + signal.SIGTERM)
        finally:
            if process.poll() is None:
                process.kill()
            process.communicate()

    def test_cleanup_kills_child_even_after_group_leader_exits(self):
        code = """
import subprocess, sys
subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'])
print('ready', flush=True)
"""
        process = subprocess.Popen([sys.executable, '-c', code], start_new_session=True,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            self.assertEqual(process.stdout.readline().strip(), 'ready')
            process.wait(timeout=3)
            run_local.stop_process_group(process)
            # The child inherits the output pipe: EOF proves it also exited.
            process.communicate(timeout=3)
        finally:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate()


if __name__ == '__main__':
    unittest.main()
