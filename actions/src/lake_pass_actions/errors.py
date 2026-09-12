class ActionError(RuntimeError):
    """A safe-to-report action failure."""


class ProtocolError(ActionError):
    """The control plane violated the child-process protocol."""


class Cancelled(ActionError):
    """The control plane cancelled the running action."""


class ApprovalExpired(ActionError):
    """The browser could no longer complete a pending manual approval."""


class OutcomeUnknown(ActionError):
    """Final confirmation may have happened, so retrying is unsafe."""
