import asyncio
import base64
import json
import unittest

from bridge import serve_client
from protocol import ServiceUnavailable, SessionConflict, SubscriptionLimit


class Writer:
    def __init__(self):
        self.data = b''
        self.closed = False

    def write(self, data):
        self.data += data

    async def drain(self):
        pass

    def close(self):
        self.closed = True


class App:
    healthy = True

    def is_healthy(self):
        return self.healthy

    def log(self, *args, **kwargs):
        pass

    async def models(self):
        return {'data': [{'id': 'account-model', 'default': True}]}

    async def quota_status(self):
        return {'available': False, 'retry_at': 123}


class HTTPTests(unittest.IsolatedAsyncioTestCase):
    async def test_subscription_status_requires_auth_and_errors_keep_code(self):
        class Stub:
            app = App()
            async def complete(self, key, body):
                raise SubscriptionLimit(123)
        stub = Stub()
        head, result = await self.request(stub, '/v1/quota', token='wrong')
        self.assertIn('401', head)
        head, result = await self.request(stub, '/v1/quota')
        self.assertIn('200', head)
        self.assertFalse(result['available'])
        head, result = await self.request(stub, '/v1/chat/completions', {})
        self.assertIn('423', head)
        self.assertNotIn('Retry-After: 1', head)
        self.assertEqual(result['error']['code'], 'codex_subscription_limit')
        self.assertEqual(result['error']['retry_at'], 123)

    async def request(self, bridge, path='/health', payload=None, token='secret'):
        reader = asyncio.StreamReader()
        writer = Writer()
        data = json.dumps(payload).encode() if payload is not None else b''
        method = 'POST' if payload is not None else 'GET'
        reader.feed_data(f'{method} {path} HTTP/1.1\r\nAuthorization: Bearer {token}\r\nContent-Length: {len(data)}\r\nX-Artex-Codex-Session: w\r\n\r\n'.encode() + data)
        await serve_client(reader, writer, bridge, 'secret')
        head, raw = writer.data.split(b'\r\n\r\n', 1)
        return head.decode(), json.loads(raw)

    async def test_health_exposes_reader_failure_and_models_require_auth(self):
        class Stub:
            app = App()
        stub = Stub()
        stub.app.healthy = False
        head, result = await self.request(stub)
        self.assertIn('503', head)
        self.assertFalse(result['ok'])
        head, _ = await self.request(stub, '/v1/models', token='wrong')
        self.assertIn('401', head)
        head, result = await self.request(stub, '/v1/models')
        self.assertIn('200', head)
        self.assertEqual(result['data'][0]['id'], 'account-model')

    async def test_retryable_and_conflict_errors_keep_status_and_usage(self):
        class Stub:
            app = App()
            error = None

            async def complete(self, key, body):
                raise self.error

        stub = Stub()
        for error in (ServiceUnavailable('retry'), SessionConflict('ambiguous result')):
            error.artex_usage = {'source': 'codex_subscription', 'known': False, 'samples': []}
            stub.error = error
            head, result = await self.request(stub, '/v1/chat/completions', {})
            self.assertIn(str(error.status), head)
            self.assertEqual(result['error']['message'], str(error))
            encoded = next(line.split(': ', 1)[1] for line in head.splitlines() if line.startswith('X-Artex-Usage:'))
            self.assertEqual(json.loads(base64.b64decode(encoded)), error.artex_usage)
            if error.status == 503:
                self.assertIn('Retry-After: 1', head)


if __name__ == '__main__':
    unittest.main()
