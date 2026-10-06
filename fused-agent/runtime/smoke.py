"""Check the packaged HTTP agent runtime without optional provider transports."""
import importlib
import os
import sys


def main():
    sys.path.insert(0, sys.argv[1])
    os.environ["LITELLM_LOCAL_MODEL_COST_MAP"] = "True"
    # Exercise runtime entry points and native dependencies. grpc belongs to
    # optional extras, not the pinned HTTP runtime shipped by Fused.
    for module in (
        "ssl", "harnest.runtime", "google.adk", "uvicorn", "httpx", "yaml",
        "graphql", "openai", "pydantic_core", "cryptography.hazmat.bindings._rust",
    ):
        importlib.import_module(module)
    print("Agent runtime imports passed")


if __name__ == "__main__":
    main()
