from harnest.lib.storage import store
from harnest import lifecycle


@lifecycle.storage.sessions
def session_store():
    """Return the shared store for completed conversations and business state."""

    return store


@lifecycle.storage.checkpoints
def checkpointer():
    """Return the same store for private in-progress execution state."""

    return store
