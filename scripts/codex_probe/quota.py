"""Minimal durable latch; reset timestamps are hints, never recovery evidence."""
import json
import os


class QuotaState:
    def __init__(self, output):
        self.path = output / 'quota-state.json'
        self.blocked = False
        self.retry_at = 0
        self.available = False
        self.generation = 0
        try:
            self.blocked = json.loads(self.path.read_text()).get('blocked') is True
        except FileNotFoundError:
            pass
        except (ValueError, AttributeError):
            self.blocked = True  # A damaged latch is not evidence of recovery.

    def save(self):
        temp = self.path.with_suffix('.tmp')
        with temp.open('w') as f:
            json.dump({'blocked': self.blocked}, f)
            f.flush()
            os.fsync(f.fileno())
        temp.replace(self.path)

    def block(self):
        self.generation += 1
        self.blocked, self.available = True, False
        self.save()

    def update(self, snapshot):
        permission = snapshot.get('ordinaryUsageAllowed')
        self.available = permission is True
        if permission is True:
            self.blocked = False
        elif permission is False:
            self.blocked = True
        buckets = [snapshot.get('rateLimits')]
        by_id = snapshot.get('rateLimitsByLimitId')
        if isinstance(by_id, dict):
            buckets.extend(by_id.values())
        resets = []
        for bucket in buckets:
            if not isinstance(bucket, dict):
                continue
            for key in ('primary', 'secondary'):
                window = bucket.get(key)
                if isinstance(window, dict):
                    used, reset = window.get('usedPercent'), window.get('resetsAt')
                    if type(used) in (int, float) and used >= 100 and type(reset) is int:
                        resets.append(reset)
        self.retry_at = max(resets, default=0)
        self.save()

    def status(self):
        return {'available': self.available, 'blocked': self.blocked,
                'retry_at': self.retry_at}
