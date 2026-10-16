"""Offline transport regressions; subprocess doubles never run inference."""
import asyncio
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from app_server import AppServer
from protocol import ProtocolError, ServiceUnavailable


class Input:
    def __init__(self, process):
        self.process = process
        self.messages = []
        self.error = None

    def write(self, data):
        if self.error:
            raise self.error
        message = json.loads(data)
        self.messages.append(message)
        if self.process.respond and 'method' in message and 'id' in message:
            result = self.process.respond(message)
            if result is not None:
                self.process.emit({'id': message['id'], 'result': result})

    async def drain(self):
        pass

    def close(self):
        pass


class Process:
    pid = 12345
    def __init__(self, respond=None):
        self.stdout = asyncio.StreamReader()
        self.stdin = Input(self)
        self.returncode = None
        self.respond = respond
        self.exited = asyncio.Event()
        self.terminated = self.killed = False

    def emit(self, message):
        self.stdout.feed_data((json.dumps(message) + '\n').encode())

    async def wait(self):
        await self.exited.wait()
        return self.returncode

    def terminate(self):
        self.terminated = True

    def kill(self):
        self.killed = True
        self.returncode = -9
        self.exited.set()


class State:
    def __init__(self):
        self.thread = 't'
        self.key = 'session'
        self.model = 'model'
        self.names = {'artex_lookup': 'lookup'}
        self.pending = {}
        self.events = asyncio.Queue()
        self.text = ''
        self.active_turn = 'turn'
        self.broken = False

    def take_text(self):
        text, self.text = self.text, ''
        return text


class AppServerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.app = AppServer(Path(self.temp.name))
        self.app.proc = Process()
        self.app.initialized = True
        self.app.reader = asyncio.create_task(self.app.read())
        self.state = State()
        self.app.sessions['t'] = self.state

    async def asyncTearDown(self):
        self.app.reader.cancel()
        await asyncio.gather(self.app.reader, return_exceptions=True)
        if getattr(self.app, 'stderr', None):
            self.app.stderr.close()
        self.temp.cleanup()

    def emit(self, method, **params):
        self.app.proc.emit({'method': method, 'params': {'threadId': 't', **params}})

    async def next_event(self):
        return await asyncio.wait_for(self.state.events.get(), .5)

    async def test_bad_lines_do_not_drop_later_notifications_or_log_payload(self):
        self.app.proc.stdout.feed_data(b'secret-token not-json\n')
        self.app.proc.emit(['secret-token'])
        self.app.proc.emit({'method': 'turn/started', 'params': []})
        self.emit('item/agentMessage/delta', delta='hello')
        self.emit('turn/completed', turn={'id': 'turn', 'status': 'completed'})
        self.assertEqual(await self.next_event(), {'kind': 'text', 'text': 'hello'})
        log = (self.app.output / 'bridge.jsonl').read_text()
        self.assertNotIn('secret-token', log)
        self.assertTrue(self.app.is_healthy())

    async def test_eof_fails_waiters_and_sessions_as_retryable(self):
        future = asyncio.get_running_loop().create_future()
        self.app.requests[1] = future
        self.app.proc.stdout.feed_eof()
        await self.app.reader
        with self.assertRaises(ServiceUnavailable):
            await future
        self.assertTrue(self.state.broken)
        self.assertTrue((await self.next_event())['retryable'])
        self.assertFalse(self.app.is_healthy())

    async def test_fatal_read_error_fails_waiting_rpc(self):
        request = asyncio.create_task(self.app.rpc('thread/start', {}))
        await asyncio.sleep(0)
        self.app.proc.stdout.set_exception(OSError('secret-payload'))
        with self.assertRaises(ServiceUnavailable):
            await request
        self.assertTrue((await self.next_event())['retryable'])
        self.assertNotIn('secret-payload', (self.app.output / 'bridge.jsonl').read_text())

    async def test_rpc_error_codes_distinguish_invalid_request_from_backend_failure(self):
        for code, expected in ((-32602, ProtocolError), (-32603, ServiceUnavailable),
                               (-32000, ServiceUnavailable)):
            request = asyncio.create_task(self.app.rpc('thread/start', {}))
            await asyncio.sleep(0)
            self.app.proc.emit({'id': self.app.seq, 'error': {'code': code, 'message': 'secret'}})
            with self.assertRaises(expected) as caught:
                await request
            self.assertEqual(type(caught.exception), expected)
            self.assertNotIn('secret', str(caught.exception))
            self.assertEqual(self.app.requests, {})

    async def test_malformed_notification_fields_do_not_stop_later_valid_delta(self):
        self.emit('turn/started', turn=None)
        self.emit('item/agentMessage/delta', delta=['secret'])
        self.emit('turn/completed', turn={'status': []})
        self.emit('item/agentMessage/delta', delta='valid')
        self.emit('turn/completed', turn={'id': 'turn', 'status': 'completed'})
        self.assertEqual(await self.next_event(), {'kind': 'text', 'text': 'valid'})
        self.assertTrue(self.app.is_healthy())

    async def test_health_rejects_finished_reader_even_with_live_child(self):
        self.app.reader.cancel()
        await asyncio.gather(self.app.reader, return_exceptions=True)
        self.assertFalse(self.app.is_healthy())

    async def test_tool_drains_preceding_text_and_final_drains_following_text(self):
        self.emit('item/agentMessage/delta', delta='before')
        self.app.proc.emit({'id': 10, 'method': 'item/tool/call', 'params': {
            'threadId': 't', 'tool': 'artex_lookup', 'callId': 'call', 'arguments': {'q': 1}}})
        self.emit('item/agentMessage/delta', delta='after')
        self.emit('turn/completed', turn={'id': 'turn', 'status': 'completed'})
        event = await self.next_event()
        self.assertEqual(event['text'], 'before')
        self.assertEqual(event['arguments'], {'q': 1})
        self.assertEqual(await self.next_event(), {'kind': 'text', 'text': 'after'})
        self.assertEqual(self.state.text, '')
        self.assertEqual(self.state.pending, {'call': 10})

    async def test_distinct_assistant_items_keep_a_text_boundary(self):
        self.emit('item/agentMessage/delta', itemId='one', delta='First paragraph.')
        self.emit('item/agentMessage/delta', itemId='two', delta='Second paragraph.')
        self.emit('turn/completed', turn={'id': 'turn', 'status': 'completed'})
        self.assertEqual(await self.next_event(), {
            'kind': 'text', 'text': 'First paragraph.\n\nSecond paragraph.'})

    async def test_invalid_account_response_is_service_unavailable_and_cleans_up(self):
        self.app.proc.returncode = 1
        self.app.proc.exited.set()
        self.app.CLOSE_TIMEOUT = .001
        spawned = Process(lambda message: [] if message['method'] == 'account/read' else {})
        async def spawn(*args, **kwargs):
            return spawned
        with patch('app_server.asyncio.create_subprocess_exec', side_effect=spawn):
            with self.assertRaises(ServiceUnavailable):
                await self.app.ensure_ready()
        self.assertFalse(self.app.is_healthy())
        self.assertTrue(spawned.killed)
        self.assertTrue(self.app.reader.done())
        self.assertIsNone(self.app.stderr)

    async def test_unknown_tool_rejected_without_forwarding(self):
        self.app.proc.emit({'id': 10, 'method': 'item/tool/call', 'params': {
            'threadId': 't', 'tool': 'shell', 'callId': 'call', 'arguments': {}}})
        event = await self.next_event()
        self.assertEqual(event['kind'], 'error')
        self.assertFalse(event['retryable'])
        self.assertFalse(self.app.proc.stdin.messages[0]['result']['success'])
        self.assertEqual(self.state.pending, {})

    async def test_duplicate_tool_call_preserves_first_rpc(self):
        for rid in (10, 11):
            self.app.proc.emit({'id': rid, 'method': 'item/tool/call', 'params': {
                'threadId': 't', 'tool': 'artex_lookup', 'callId': 'call', 'arguments': {}}})
        self.assertEqual((await self.next_event())['kind'], 'tool')
        self.assertEqual((await self.next_event())['kind'], 'error')
        self.assertEqual(self.state.pending, {'call': 10})
        self.assertEqual(self.app.proc.stdin.messages[-1]['id'], 11)
        self.assertFalse(self.app.proc.stdin.messages[-1]['result']['success'])

    async def test_failed_turn_marks_only_transient_backend_errors_retryable(self):
        cases = [
            ('rateLimitExceeded', True), ('flexUnavailable', True),
            ('serverOverloaded', True), ('internalServerError', True),
            ('unauthorized', False), ('badRequest', False),
            ('contextWindowExceeded', False), ('contentPolicyViolation', False),
            ('usageLimitExceeded', False), ('sessionBudgetExceeded', False),
            ('unknown', False), (None, False),
        ]
        for name in ('httpConnectionFailed', 'responseStreamConnectionFailed',
                     'responseStreamDisconnected', 'responseTooManyFailedAttempts'):
            cases.extend([({name: {'httpStatusCode': code}}, retryable) for code, retryable in
                          ((None, True), (408, True), (429, True), (500, True),
                           (503, True), (599, True), (400, False), (401, False),
                           (403, False), (404, False), (600, False), ('503', False))])
        for info, expected in cases:
            with self.subTest(info=info):
                self.state.text = 'partial text'
                self.emit('turn/completed', turn={'id': 'turn', 'status': 'failed',
                    'error': {'message': 'secret backend content', 'codexErrorInfo': info}})
                event = await self.next_event()
                self.assertEqual(event['kind'], 'error')
                self.assertEqual(event['retryable'], expected)
                self.assertNotIn('secret', event['error'])
                self.assertEqual(self.state.text, '')
        self.emit('turn/completed', turn={'id': 'turn', 'status': 'interrupted',
            'error': {'codexErrorInfo': 'serverOverloaded'}})
        self.assertFalse((await self.next_event())['retryable'])

    async def test_rpc_nested_error_metadata_prevents_retrying_auth_or_policy(self):
        for info, expected in (('unauthorized', ProtocolError),
                               ('contentPolicyViolation', ProtocolError),
                               ('unknown', ProtocolError),
                               ({'httpConnectionFailed': {'httpStatusCode': 403}}, ProtocolError),
                               ('serverOverloaded', ServiceUnavailable)):
            with self.subTest(info=info):
                request = asyncio.create_task(self.app.rpc('thread/start', {}))
                await asyncio.sleep(0)
                self.app.proc.emit({'id': self.app.seq, 'error': {
                    'code': -32000, 'message': 'secret', 'data': {'codexErrorInfo': info}}})
                with self.assertRaises(expected) as caught:
                    await request
                self.assertEqual(type(caught.exception), expected)

    async def test_pending_tool_limit_rejects_excess_without_growing_queue(self):
        self.app.MAX_PENDING_TOOLS = 2
        for rid in range(10, 14):
            self.app.proc.emit({'id': rid, 'method': 'item/tool/call', 'params': {
                'threadId': 't', 'tool': 'artex_lookup', 'callId': str(rid), 'arguments': {}}})
        self.assertEqual((await self.next_event())['kind'], 'tool')
        self.assertEqual((await self.next_event())['kind'], 'tool')
        event = await self.next_event()
        self.assertEqual(event['kind'], 'error')
        self.assertFalse(event['retryable'])
        self.assertEqual(self.state.pending, {'10': 10, '11': 11})
        self.assertFalse(self.app.proc.stdin.messages[0]['result']['success'])
        self.assertTrue(self.state.events.empty())
        self.assertTrue(self.state.broken)

    async def test_broken_pipe_becomes_service_unavailable_and_clears_request(self):
        self.app.proc.stdin.error = BrokenPipeError('secret-payload')
        with self.assertRaises(ServiceUnavailable):
            await self.app.rpc('thread/start', {})
        self.assertEqual(self.app.requests, {})
        self.assertFalse(self.app.is_healthy())

    async def test_rpc_timeout_becomes_service_unavailable(self):
        self.app.RPC_TIMEOUT = .01
        with self.assertRaises(ServiceUnavailable):
            await asyncio.wait_for(self.app.rpc('thread/start', {}), .2)
        self.assertEqual(self.app.requests, {})

    async def test_models_collect_pages_and_prefer_model_identifier(self):
        def respond(message):
            self.assertEqual(message['method'], 'model/list')
            if message['params'].get('cursor'):
                self.assertEqual(message['params']['cursor'], 'page2')
                return {'data': [{'id': 'fallback', 'isDefault': False}], 'nextCursor': None}
            return {'data': [{'id': 'label', 'model': 'real-model', 'isDefault': True}], 'nextCursor': 'page2'}
        self.app.proc.respond = respond
        self.assertEqual(await self.app.models(), {'data': [
            {'id': 'real-model', 'default': True}, {'id': 'fallback', 'default': False}]})

    async def test_close_escalates_to_kill_and_finishes_reader(self):
        self.app.CLOSE_TIMEOUT = .01
        await asyncio.wait_for(self.app.close(), .2)
        self.assertTrue(self.app.proc.terminated)
        self.assertTrue(self.app.proc.killed)
        self.assertTrue(self.app.reader.done())
        self.assertFalse(self.app.is_healthy())

    async def test_concurrent_recovery_starts_only_one_authenticated_process(self):
        self.app.proc.returncode = 1
        self.app.proc.exited.set()
        initial_generation = getattr(self.app, 'generation', 0)
        spawned = []
        async def spawn(*args, **kwargs):
            def respond(message):
                return {'account': {'type': 'chatgpt'}} if message['method'] == 'account/read' else {}
            process = Process(respond)
            spawned.append(process)
            return process
        with patch('app_server.asyncio.create_subprocess_exec', side_effect=spawn):
            await asyncio.gather(self.app.ensure_ready(), self.app.ensure_ready())
        self.assertEqual(len(spawned), 1)
        self.assertEqual(self.app.generation, initial_generation + 1)
        self.assertTrue(self.app.is_healthy())
        self.assertTrue(self.state.broken)


if __name__ == '__main__':
    unittest.main()
