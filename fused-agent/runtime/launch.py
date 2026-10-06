"""Start the compiled agent with its private, pinned runtime and local TLS."""
import os
from pathlib import Path
import signal
import sys
import threading


def main():
    """Keep runtime imports isolated and terminate when the owning Engine exits."""
    packages, artifact, port, cert, key = sys.argv[1:]
    sys.path.insert(0, packages)
    sys.path.insert(0, artifact)
    os.environ["PYTHONDONTWRITEBYTECODE"] = "1"
    os.environ["LITELLM_LOCAL_MODEL_COST_MAP"] = "True"

    def watch_parent():
        sys.stdin.buffer.read()
        os.kill(os.getpid(), signal.SIGTERM)

    threading.Thread(target=watch_parent, daemon=True).start()
    import uvicorn
    from harnest.runtime import create_fastapi_app
    app = create_fastapi_app(Path(artifact), playground_enabled=False,
                             openapi_enabled=False, live_enabled=False)
    uvicorn.run(app, host="127.0.0.1", port=int(port), access_log=False,
                log_level="warning", ssl_certfile=cert, ssl_keyfile=key)


if __name__ == "__main__":
    main()
