"""One shared Harnest store for sessions and private checkpoints."""

import os

from harnest.store import MemoryStore, PostgresStore


_database_url = os.getenv("HARNEST_DATABASE_URL", "").strip()
# Standalone deployments may opt into durable storage; the bundled Engine keeps its database credentials isolated.
store = PostgresStore(_database_url) if _database_url else MemoryStore()
