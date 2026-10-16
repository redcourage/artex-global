"""Codex App Server process and JSON-RPC transport."""
import asyncio
import json
import os
import time

from protocol import ProtocolError, ServiceUnavailable


def retryable_codex_error(info):
    """Only documented transient failures permit automatic retry."""
    if isinstance(info, str):
        return info in {'rateLimitExceeded', 'flexUnavailable',
                        'serverOverloaded', 'internalServerError'}
    if not isinstance(info, dict) or len(info) != 1:
        return False
    name, details = next(iter(info.items()))
    if name not in {'httpConnectionFailed', 'responseStreamConnectionFailed',
                    'responseStreamDisconnected', 'responseTooManyFailedAttempts'}:
        return False
    if not isinstance(details, dict):
        return False
    status = details.get('httpStatusCode')
    return status is None or (type(status) is int and
                              (status in (408, 429) or 500 <= status < 600))


class AppServer:
    RPC_TIMEOUT = 45
    CLOSE_TIMEOUT = 5
    MAX_PENDING_TOOLS = 256

    def __init__(self, output):
        self.output = output
        self.seq = 0
        self.requests = {}
        self.sessions = {}
        self.proc = self.reader = self.stderr = None
        self.initialized = False
        self.generation = 0
        self._broken = False
        self._ready_lock = asyncio.Lock()

    def log(self, event, **fields):
        row = {'time': time.time(), 'event': event, **fields}
        with (self.output / 'bridge.jsonl').open('a') as f:
            f.write(json.dumps(row, ensure_ascii=False) + '\n')
        print(json.dumps(row, ensure_ascii=False), flush=True)

    def is_healthy(self):
        return bool(self.initialized and not self._broken and self.proc is not None
                    and self.proc.returncode is None and self.reader is not None
                    and not self.reader.done())

    async def start(self):
        await self.ensure_ready()

    async def ensure_ready(self):
        async with self._ready_lock:
            if self.is_healthy():
                return
            await self.close()
            try:
                await self._start()
            except BaseException as exc:
                await self.close()
                if isinstance(exc, (OSError, asyncio.TimeoutError)):
                    raise ServiceUnavailable('Codex process could not start') from exc
                raise

    async def _start(self):
        self.initialized = False
        self._broken = False
        self.generation += 1
        env = os.environ.copy()
        for key in ('OPENAI_API_KEY', 'CODEX_API_KEY', 'CODEX_ACCESS_TOKEN', 'ARTEX_BRIDGE_TOKEN'):
            env.pop(key, None)
        self.stderr = (self.output / 'codex.stderr.log').open('ab')
        self.proc = await asyncio.create_subprocess_exec(
            'codex', 'app-server', '--listen', 'stdio://',
            '--disable', 'shell_tool', '--disable', 'multi_agent',
            stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE,
            stderr=self.stderr, env=env, cwd=self.output, limit=8*1024*1024)
        self.reader = asyncio.create_task(self.read())
        self.log('process', pid=self.proc.pid, generation=self.generation)
        await self.rpc('initialize', {'clientInfo': {'name': 'artex_integration_probe', 'version': '0.1'},
                                     'capabilities': {'experimentalApi': True}})
        await self.send({'method': 'initialized', 'params': {}})
        result = await self.rpc('account/read', {'refreshToken': False})
        if not isinstance(result, dict):
            raise ServiceUnavailable('Invalid Codex account response')
        account = result.get('account') or {}
        if not isinstance(account, dict):
            raise ServiceUnavailable('Invalid Codex account response')
        if account.get('type') != 'chatgpt':
            raise ProtocolError('This probe requires an existing ChatGPT login')
        self.initialized = True
        if not self.is_healthy():
            raise ServiceUnavailable('Codex disconnected during initialization')
        self.log('auth', type=account['type'], plan=account.get('planType'))

    def _fail(self, reason):
        self._broken = True
        self.initialized = False
        for fut in list(self.requests.values()):
            if not fut.done():
                fut.set_exception(ServiceUnavailable(reason))
        for state in list(self.sessions.values()):
            if not state.broken:
                state.broken = True
                state.active_turn = None
                state.events.put_nowait({'kind': 'error', 'error': reason, 'retryable': True})

    async def send(self, message):
        if self._broken or self.proc is None or self.proc.returncode is not None:
            self._fail('Codex process disconnected')
            raise ServiceUnavailable('Codex process disconnected')
        try:
            self.proc.stdin.write((json.dumps(message, ensure_ascii=False) + '\n').encode())
            await asyncio.wait_for(self.proc.stdin.drain(), self.RPC_TIMEOUT)
        except (OSError, asyncio.TimeoutError) as exc:
            self._fail('Codex transport unavailable')
            raise ServiceUnavailable('Codex transport unavailable') from exc

    async def rpc(self, method, params):
        self.seq += 1
        rid = self.seq
        fut = asyncio.get_running_loop().create_future()
        self.requests[rid] = fut
        try:
            await self.send({'id': rid, 'method': method, 'params': params})
            return await asyncio.wait_for(fut, self.RPC_TIMEOUT)
        except asyncio.TimeoutError as exc:
            self._fail('Codex request timed out')
            raise ServiceUnavailable('Codex request timed out') from exc
        finally:
            self.requests.pop(rid, None)
            if not fut.done():
                fut.cancel()
            elif not fut.cancelled():
                # send() may fail every waiter before this coroutine awaits its own.
                fut.exception()

    async def models(self):
        await self.ensure_ready()
        models, cursors = [], set()
        cursor = None
        while True:
            page = await self.rpc('model/list', {'cursor': cursor})
            if not isinstance(page, dict) or not isinstance(page.get('data'), list):
                raise ServiceUnavailable('Invalid Codex model list')
            for model in page['data']:
                if not isinstance(model, dict):
                    raise ServiceUnavailable('Invalid Codex model entry')
                model_id = model.get('model') or model.get('id')
                if not isinstance(model_id, str) or not model_id:
                    raise ServiceUnavailable('Invalid Codex model identifier')
                models.append({'id': model_id, 'default': bool(model.get('isDefault', False))})
            cursor = page.get('nextCursor')
            if not cursor:
                return {'data': models}
            if not isinstance(cursor, str) or cursor in cursors:
                raise ServiceUnavailable('Invalid Codex model pagination')
            cursors.add(cursor)

    async def read(self):
        try:
            while line := await self.proc.stdout.readline():
                try:
                    message = json.loads(line)
                    if not isinstance(message, dict):
                        raise ValueError('Expected an object')
                    await self._dispatch(message)
                except ServiceUnavailable:
                    raise
                except (ValueError, TypeError, KeyError, AttributeError) as exc:
                    # Only record the category: raw lines and exception messages
                    # can include credentials, prompts, or malformed tool content.
                    self.log('reader_invalid_message', reason=type(exc).__name__)
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            self.log('reader_error', reason=type(exc).__name__)
        finally:
            self._fail('Codex process disconnected')

    async def _reject_tool(self, rid, state, reason):
        await self.send({'id': rid, 'result': {'success': False,
            'contentItems': [{'type': 'inputText', 'text': reason}]}})
        state.events.put_nowait({'kind': 'error', 'error': reason, 'retryable': False})

    async def _dispatch(self, message):
        if 'method' not in message:
            fut = self.requests.get(message.get('id'))
            if fut is None or fut.done():
                return
            if 'error' in message:
                error = message['error']
                code = error.get('code') if isinstance(error, dict) else None
                data = error.get('data') if isinstance(error, dict) else None
                retryable = code not in (-32600, -32601, -32602)
                if isinstance(data, dict) and 'codexErrorInfo' in data:
                    retryable = retryable and retryable_codex_error(data['codexErrorInfo'])
                cls = ServiceUnavailable if retryable else ProtocolError
                fut.set_exception(cls('Codex RPC rejected the request' if cls is ProtocolError
                                      else 'Codex RPC service error'))
            elif 'result' in message:
                fut.set_result(message['result'])
            else:
                raise ValueError('Invalid RPC response')
            return
        method = message['method']
        p = message.get('params', {})
        if not isinstance(method, str) or not isinstance(p, dict):
            raise ValueError('Invalid notification')
        state = self.sessions.get(p.get('threadId'))
        if 'id' in message:
            if method != 'item/tool/call' or state is None or state.broken:
                await self.send({'id': message['id'], 'error': {
                    'code': -32601, 'message': 'Only ARTEX dynamic tools are allowed'}})
                return
            name, call_id, arguments = p.get('tool'), p.get('callId'), p.get('arguments')
            if not isinstance(name, str) or name not in state.names:
                await self._reject_tool(message['id'], state, 'Tool not exposed by ARTEX')
            elif not isinstance(call_id, str) or not call_id or not isinstance(arguments, dict):
                await self._reject_tool(message['id'], state, 'Invalid dynamic tool call')
            elif call_id in state.pending:
                await self._reject_tool(message['id'], state, 'Duplicate dynamic tool call')
            elif len(state.pending) >= self.MAX_PENDING_TOOLS:
                # Stop accepting this session's requests after one terminal event,
                # so repeated excess calls cannot grow its event queue either.
                state.broken = True
                await self._reject_tool(message['id'], state, 'Too many pending dynamic tool calls')
            else:
                state.pending[call_id] = message['id']
                state.events.put_nowait({'kind': 'tool', 'id': call_id,
                    'name': state.names[name], 'arguments': arguments, 'text': state.take_text()})
            return
        if state is None or state.broken:
            return
        if method == 'thread/tokenUsage/updated':
            try:
                total = p['tokenUsage']['total']
                state.usage.update(total)
                self.log('usage', session=state.key, thread=state.thread,
                         model=state.model, total=total)
            except (KeyError, TypeError, AttributeError, ProtocolError) as exc:
                state.usage_invalid = True
                self.log('usage_error', session=state.key, reason=type(exc).__name__)
        elif method == 'turn/started':
            turn_id = p['turn']['id']
            if not isinstance(turn_id, str):
                raise ValueError('Invalid turn identifier')
            state.active_turn = turn_id
        elif method == 'item/agentMessage/delta':
            delta = p['delta']
            if not isinstance(delta, str):
                raise ValueError('Invalid text delta')
            item_id = p.get('itemId')
            previous_item = getattr(state, '_text_item', None)
            if state.text and item_id and previous_item and item_id != previous_item:
                state.text += '\n\n'
            state._text_item = item_id
            state.text += delta
        elif method == 'turn/completed':
            turn = p['turn']
            status = turn['status']
            if not isinstance(status, str):
                raise ValueError('Invalid turn status')
            state.active_turn = None
            text = state.take_text()
            if status == 'completed':
                state.events.put_nowait({'kind': 'text', 'text': text})
            else:
                error = turn.get('error')
                info = error.get('codexErrorInfo') if isinstance(error, dict) else None
                state.events.put_nowait({'kind': 'error',
                    'error': 'Codex turn did not complete',
                    'retryable': status == 'failed' and retryable_codex_error(info)})

    async def close(self):
        self._fail('Codex process disconnected')
        proc, reader = self.proc, self.reader
        try:
            if proc is not None and proc.returncode is None:
                if proc.stdin is not None:
                    try:
                        proc.stdin.close()
                    except OSError:
                        pass
                for action in (None, proc.terminate, proc.kill):
                    if action:
                        try:
                            action()
                        except ProcessLookupError:
                            break
                    try:
                        await asyncio.wait_for(proc.wait(), self.CLOSE_TIMEOUT)
                        break
                    except asyncio.TimeoutError:
                        continue
        finally:
            if reader is not None and reader is not asyncio.current_task():
                reader.cancel()
                await asyncio.gather(reader, return_exceptions=True)
            if self.stderr is not None:
                self.stderr.close()
                self.stderr = None
