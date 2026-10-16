"""Bridge errors mapped to explicit HTTP status codes."""


class ProtocolError(ValueError):
    status = 400


class ServiceUnavailable(ProtocolError):
    status = 503


class SessionConflict(ProtocolError):
    status = 409
