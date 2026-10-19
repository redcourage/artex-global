import asyncio
import copy
from pathlib import Path
import unittest

from bridge import Bridge, ProtocolError, UsageTracker, added_messages, dynamic_tools, completion, pending_results


class ProtocolTests(unittest.TestCase):
    def test_usage_snapshots_are_deltas_and_duplicate_events_are_ignored(self):
        usage = UsageTracker('thread-1', 'gpt-6-luna')
        first = {'inputTokens': 100, 'cachedInputTokens': 60, 'cacheWriteInputTokens': 10,
                 'outputTokens': 20, 'reasoningOutputTokens': 5, 'totalTokens': 120}
        usage.update(first)
        usage.update(first)
        sample = usage.drain()
        self.assertEqual(len(sample), 1)
        self.assertEqual(sample[0]['input_tokens'], 100)
        self.assertEqual(sample[0]['cache_read_tokens'], 60)
        self.assertEqual(sample[0]['cache_write_tokens'], 10)
        self.assertEqual(sample[0]['reasoning_tokens'], 5)
        self.assertEqual(usage.drain(), [])
        usage.update({**first, 'inputTokens': 250, 'cachedInputTokens': 160, 'outputTokens': 30, 'totalTokens': 280})
        second = usage.drain()[0]
        self.assertEqual(second['input_tokens'], 150)
        self.assertEqual(second['cache_read_tokens'], 100)
        self.assertNotEqual(sample[0]['id'], second['id'])

    def test_usage_counter_regression_or_invalid_cache_is_not_priced(self):
        usage = UsageTracker('thread-1', 'model')
        first = {'inputTokens':100,'cachedInputTokens':50,'outputTokens':20,'reasoningOutputTokens':0}
        usage.update(first)
        with self.assertRaises(ProtocolError):
            usage.update({**first,'inputTokens':90})
        with self.assertRaises(ProtocolError):
            UsageTracker('thread-2','model').update({**first,'cachedInputTokens':101})
        with self.assertRaises(ProtocolError):
            UsageTracker('thread-3','model').update({})

    def test_tools_are_namespaced_and_schema_is_preserved(self):
        tools = [{'type': 'function', 'function': {'name': 'Read', 'description': 'Read local file',
                  'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}}}}}]
        specs, names = dynamic_tools(tools)
        self.assertEqual(specs[0]['name'], 'artex_Read')
        self.assertEqual(specs[0]['inputSchema']['properties'], {'path': {'type': 'string'}})
        self.assertEqual(names, {'artex_Read': 'Read'})

    def test_tool_completion_preserves_correlation_and_arguments(self):
        msg = completion('model-a', {'kind': 'tool', 'id': 'call-7', 'name': 'Read',
                                     'arguments': {'path': '/tmp/fixture'}})
        choice = msg['choices'][0]
        self.assertEqual(choice['finish_reason'], 'tool_calls')
        self.assertEqual(choice['message']['tool_calls'][0]['id'], 'call-7')
        self.assertEqual(choice['message']['tool_calls'][0]['function']['arguments'], '{"path": "/tmp/fixture"}')

    def test_continuation_excludes_prior_messages(self):
        old = [{'role': 'user', 'content': 'first'}]
        current = old + [{'role': 'assistant', 'content': 'answer'}, {'role': 'user', 'content': 'second'}]
        self.assertEqual(added_messages(old, current), current[1:])

    def test_replaced_history_is_rejected_instead_of_mixed(self):
        with self.assertRaises(ProtocolError):
            added_messages([{'role': 'user', 'content': 'task A'}], [{'role': 'user', 'content': 'task B'}])

    def test_resumed_tool_arguments_allow_json_serialization_changes(self):
        old = [{'role': 'assistant', 'tool_calls': [{'id': 'read-1', 'type': 'function',
                'function': {'name': 'Read', 'arguments': '{"path": "한글", "options": {"lines": [1, 2]}}'}}]}]
        current = copy.deepcopy(old)
        current[0]['tool_calls'][0]['function']['arguments'] = (
            '{"options":{"lines":[1,2]},"path":"\\ud55c\\uae00"}')
        current.append({'role': 'user', 'content': 'continue'})
        self.assertEqual(added_messages(old, current), [current[-1]])
        self.assertEqual(old[0]['tool_calls'][0]['function']['arguments'],
                         '{"path": "한글", "options": {"lines": [1, 2]}}')

    def test_resumed_history_still_rejects_changed_tool_data(self):
        old = [{'role': 'assistant', 'tool_calls': [{'id': 'read-1', 'type': 'function',
                'function': {'name': 'Read', 'arguments': '{"value": 1}'}}]},
               {'role': 'tool', 'tool_call_id': 'read-1', 'content': 'original'}]
        for replacement in ('{"value":2}', '{"value":true}', '{"value":"1"}'):
            with self.subTest(arguments=replacement):
                current = copy.deepcopy(old)
                current[0]['tool_calls'][0]['function']['arguments'] = replacement
                with self.assertRaises(ProtocolError):
                    added_messages(old, current)
        current = copy.deepcopy(old)
        current[1]['content'] = 'changed'
        with self.assertRaises(ProtocolError):
            added_messages(old, current)

    def test_tool_results_match_pending_ids_and_duplicates_fail(self):
        messages = [{'role': 'tool', 'tool_call_id': 'call-7', 'content': 'file contents'}]
        self.assertEqual(pending_results(messages, {'call-7': 42}), [(42, 'file contents')])
        with self.assertRaises(ProtocolError):
            pending_results(messages + messages, {'call-7': 42})

    def test_unmatched_tool_result_fails(self):
        with self.assertRaises(ProtocolError):
            pending_results([{'role': 'tool', 'tool_call_id': 'other', 'content': 'x'}], {'call-7': 42})

    def test_plain_completion_does_not_invent_usage(self):
        msg = completion('model-a', {'kind': 'text', 'text': 'done'})
        self.assertEqual(msg['choices'][0]['finish_reason'], 'stop')
        self.assertEqual(msg['choices'][0]['message']['content'], 'done')
        self.assertNotIn('usage', msg)


class FakeApp:
    """Deterministic transport peer; no Codex login or quota needed."""
    output = Path('/tmp')

    def __init__(self, finish=True):
        self.sessions = {}
        self.calls = []
        self.finish = finish
        self.started = asyncio.Event()

    def log(self, *args, **kwargs):
        pass

    async def rpc(self, method, params):
        self.calls.append((method, params))
        if method == 'thread/start':
            return {'model': params['model'], 'thread': {'id': 'thread-' + str(len(self.calls))}}
        if method == 'turn/start':
            state = self.sessions[params['threadId']]
            # The reader can consume completion before the RPC caller resumes.
            if self.finish:
                state.active_turn = None
                await state.events.put({'kind': 'text', 'text': 'answer'})
            self.started.set()
            return {'turn': {'id': 'turn-' + str(len(self.calls))}}
        if method == 'turn/interrupt':
            return {}
        raise AssertionError('Unexpected RPC: ' + method)


class SessionTests(unittest.IsolatedAsyncioTestCase):
    def body(self, messages):
        return {'model': 'test-model', 'messages': messages,
                'tools': [{'type': 'function', 'function': {'name': 'Read', 'parameters': {'type': 'object'}}}], 'stream': False}

    async def test_fast_completion_clears_active_turn_and_next_user_reuses_thread(self):
        app = FakeApp()
        bridge = Bridge(app)
        messages = [{'role': 'user', 'content': 'first'}]
        first = await bridge.complete('worker-1', self.body(messages))
        self.assertIsNone(bridge.states['worker-1'].active_turn)
        messages += [first['choices'][0]['message'], {'role': 'user', 'content': 'second'}]
        await bridge.complete('worker-1', self.body(messages))
        starts = [p for m, p in app.calls if m == 'turn/start']
        self.assertEqual(len(starts), 2)
        self.assertEqual(starts[0]['threadId'], starts[1]['threadId'])
        self.assertEqual(starts[1]['input'][0]['text'], 'second')

    async def test_disconnect_retains_request_until_explicit_cleanup(self):
        app = FakeApp(finish=False)
        bridge = Bridge(app)
        task = asyncio.create_task(bridge.complete('worker-1', self.body([{'role': 'user', 'content': 'run'}])))
        await app.started.wait()
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assertEqual([m for m, _ in app.calls].count('turn/interrupt'), 0)
        self.assertFalse(bridge.locks)
        await bridge.close()
        self.assertEqual([m for m, _ in app.calls].count('turn/interrupt'), 1)
        self.assertFalse(bridge.states)
        self.assertFalse(app.sessions)

    async def test_cancellation_before_start_ack_still_interrupts_started_turn(self):
        class DelayedApp(FakeApp):
            def __init__(self):
                super().__init__(finish=False)
                self.ack = asyncio.Event()

            async def rpc(self, method, params):
                result = await super().rpc(method, params)
                if method == 'turn/start':
                    await self.ack.wait()
                return result

        app = DelayedApp()
        bridge = Bridge(app)
        task = asyncio.create_task(bridge.complete('worker-1', self.body([{'role': 'user', 'content': 'run'}])))
        await app.started.wait()
        task.cancel()
        await asyncio.sleep(0)
        app.ack.set()
        with self.assertRaises(asyncio.CancelledError):
            await task
        await bridge.close()
        self.assertEqual([m for m, _ in app.calls].count('turn/interrupt'), 1)
        self.assertFalse(bridge.states)

    async def test_distinct_workers_do_not_share_threads(self):
        app = FakeApp()
        bridge = Bridge(app)
        body = self.body([{'role': 'user', 'content': 'run'}])
        await asyncio.gather(bridge.complete('worker-1', body), bridge.complete('worker-2', body))
        self.assertNotEqual(bridge.states['worker-1'].thread, bridge.states['worker-2'].thread)

    async def test_identical_retry_does_not_start_duplicate_inference(self):
        app = FakeApp()
        bridge = Bridge(app)
        body = self.body([{'role': 'user', 'content': 'run'}])
        first = await bridge.complete('worker-1', body)
        count = len(app.calls)
        self.assertEqual(first, await bridge.complete('worker-1', body))
        self.assertEqual(count, len(app.calls))

    async def test_completion_reports_usage_and_retry_keeps_same_metering_ids(self):
        class MeteredApp(FakeApp):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    self.sessions[params['threadId']].usage.update({
                        'inputTokens':100,'cachedInputTokens':40,'cacheWriteInputTokens':10,
                        'outputTokens':20,'reasoningOutputTokens':5,'totalTokens':120})
                return await super().rpc(method, params)
        app = MeteredApp()
        bridge = Bridge(app)
        body = self.body([{'role':'user','content':'run'}])
        first = await bridge.complete('worker-1',body)
        self.assertEqual(first['usage']['prompt_tokens'],100)
        self.assertEqual(first['usage']['prompt_tokens_details']['cache_write_tokens'],10)
        self.assertEqual(first['usage']['completion_tokens'],20)
        self.assertEqual(first['artex_usage']['source'],'codex_subscription')
        self.assertEqual(first,await bridge.complete('worker-1',body))

    async def test_later_turn_without_usage_is_unknown_not_free(self):
        class MeteredOnce(FakeApp):
            async def rpc(self, method, params):
                if method == 'turn/start':
                    self.sessions[params['threadId']].usage.update({
                        'inputTokens':100,'cachedInputTokens':40,'outputTokens':20,'reasoningOutputTokens':0})
                return await super().rpc(method, params)
        bridge=Bridge(MeteredOnce())
        messages=[{'role':'user','content':'first'}]
        first=await bridge.complete('worker-1',self.body(messages))
        messages += [first['choices'][0]['message'],{'role':'user','content':'second'}]
        second=await bridge.complete('worker-1',self.body(messages))
        self.assertEqual(second['artex_usage']['samples'], [])
        self.assertEqual(len(second['artex_usage']['missing_ids']), 1)
        self.assertNotIn('usage',second)

    async def test_tools_defer_accounting_until_terminal_usage_arrives(self):
        class ToolApp(FakeApp):
            async def rpc(self,method,params):
                if method=='turn/start':
                    self.calls.append((method,params))
                    state=self.sessions[params['threadId']]
                    state.pending['call-1']=10
                    await state.events.put({'kind':'tool','id':'call-1','name':'Read','arguments':{}})
                    return {'turn':{'id':'turn-1'}}
                return await super().rpc(method,params)

            async def send(self,message):
                state=next(iter(self.sessions.values()))
                state.usage.update({'inputTokens':100,'cachedInputTokens':40,'outputTokens':20,'reasoningOutputTokens':0})
                state.active_turn=None
                await state.events.put({'kind':'text','text':'done'})

        bridge=Bridge(ToolApp())
        messages=[{'role':'user','content':'run'}]
        first=await bridge.complete('worker-1',self.body(messages))
        self.assertTrue(first['artex_usage']['deferred'])
        self.assertNotIn('usage',first)
        messages += [first['choices'][0]['message'],{'role':'tool','tool_call_id':'call-1','content':'result'}]
        last=await bridge.complete('worker-1',self.body(messages))
        self.assertTrue(last['artex_usage']['known'])
        self.assertEqual(last['usage']['prompt_tokens'],100)

    async def test_failed_turn_preserves_reported_usage(self):
        class FailedApp(FakeApp):
            async def rpc(self,method,params):
                if method=='turn/start':
                    state=self.sessions[params['threadId']]
                    state.usage.update({'inputTokens':100,'cachedInputTokens':40,'outputTokens':20,'reasoningOutputTokens':0})
                    await state.events.put({'kind':'error','error':'backend failed after usage'})
                    return {'turn':{'id':'turn-1'}}
                return await super().rpc(method,params)
        bridge=Bridge(FailedApp())
        with self.assertRaises(ProtocolError) as caught:
            await bridge.complete('worker-1',self.body([{'role':'user','content':'run'}]))
        self.assertTrue(caught.exception.artex_usage['known'])
        self.assertEqual(caught.exception.artex_usage['samples'][0]['input_tokens'],100)


if __name__ == '__main__':
    unittest.main()
