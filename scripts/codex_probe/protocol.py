"""Bridge errors mapped to explicit HTTP status codes."""


class ProtocolError(ValueError):
    status = 400


class ServiceUnavailable(ProtocolError):
    status = 503


class SessionConflict(ProtocolError):
    status = 409


class SubscriptionLimit(ProtocolError):
    # 423 deliberately bypasses ordinary 429 retries and paid-provider failover.
    status = 423

    def __init__(self, retry_at=0):
        super().__init__('Codex subscription limit reached; waiting for confirmed recovery')
        self.retry_at = retry_at


class ToolReviewRequired(SessionConflict):
    code = 'tool_resume_review_required'
